package store

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/dkmr27/Rradmon/internal/reticulum"
)

// Read-side queries for the web UI. Hashes are returned as lowercase hex.

func hx(b []byte) string { return hex.EncodeToString(b) }

func fromMs(v int64) time.Time { return time.UnixMilli(v) }

// Summary is the header strip of the UI.
type Summary struct {
	Packets      int64         `json:"packets"`
	Receptions   int64         `json:"receptions"`
	Nodes        int64         `json:"nodes"`
	Destinations int64         `json:"destinations"`
	Packets1h    int64         `json:"packets_1h"`
	Orphans1h    int64         `json:"orphans_1h"`
	Observers    []ObserverRow `json:"observers"`
}

type ObserverRow struct {
	ID         int64     `json:"id"`
	ExtID      string    `json:"ext_id"`
	Source     string    `json:"source"`
	LastSeen   time.Time `json:"last_seen"`
	UtilShort  *float64  `json:"util_s"`
	UtilLong   *float64  `json:"util_l"`
	NoiseFloor *float64  `json:"noise_floor"`
	// Local: an RNode attached to this server ("us"), rather than a remote
	// observer reporting over MQTT
	Local bool `json:"local"`
}

// LocalSource is the source of observers attached to this server.
const LocalSource = "rnode-serial"

// isLocal: an observer is "local" (us) if it is an RNode attached to this
// server, or the MQTT observer named by the observer.local setting
// (localExt). Read the setting before a query, not inside its row loop: the
// store has one connection.
func isLocal(source, extID, localExt string) bool {
	return source == LocalSource || (extID != "" && extID == localExt)
}

func (s *Store) Summary(now time.Time) (Summary, error) {
	localExt := s.Setting("observer.local")
	var sm Summary
	hour := ms(now.Add(-time.Hour))
	err := s.DB.QueryRow(`SELECT
		(SELECT COUNT(*) FROM packets),
		(SELECT COUNT(*) FROM receptions),
		(SELECT COUNT(DISTINCT identity_hash) FROM destinations) +
		  (SELECT COUNT(DISTINCT tx_identity) FROM receptions WHERE tx_identity IS NOT NULL
		     AND tx_identity NOT IN (SELECT identity_hash FROM destinations WHERE identity_hash IS NOT NULL)),
		(SELECT COUNT(*) FROM destinations),
		(SELECT COUNT(*) FROM packets WHERE first_seen >= ?),
		(SELECT COUNT(*) FROM frame_errors WHERE t >= ? AND matched_packet_id IS NULL)`,
		hour, hour).Scan(&sm.Packets, &sm.Receptions, &sm.Nodes, &sm.Destinations, &sm.Packets1h, &sm.Orphans1h)
	if err != nil {
		return sm, err
	}
	rows, err := s.DB.Query(`SELECT o.id, o.ext_id, COALESCE(o.source,''), o.last_seen,
		c.util_s, c.util_l, c.noise_floor
		FROM observers o
		LEFT JOIN channel_stats c ON c.rowid = (SELECT rowid FROM channel_stats
			WHERE observer_id = o.id ORDER BY t DESC LIMIT 1)
		ORDER BY (o.source = 'rnode-serial' OR o.ext_id = ?) DESC, o.ext_id`, localExt)
	if err != nil {
		return sm, err
	}
	defer rows.Close()
	for rows.Next() {
		var o ObserverRow
		var last int64
		if err := rows.Scan(&o.ID, &o.ExtID, &o.Source, &last, &o.UtilShort, &o.UtilLong, &o.NoiseFloor); err != nil {
			return sm, err
		}
		o.Local = isLocal(o.Source, o.ExtID, localExt)
		o.LastSeen = fromMs(last)
		sm.Observers = append(sm.Observers, o)
	}
	return sm, rows.Err()
}

// FeedRow is one reception (or unmatched orphan) for the packet feed.
type FeedRow struct {
	Kind       string    `json:"kind"` // "rx" or "orphan"
	ID         int64     `json:"id"`   // reception id or frame error id
	PacketID   int64     `json:"packet_id,omitempty"`
	T          time.Time `json:"t"`
	Observer   string    `json:"observer"`
	RSSI       *float64  `json:"rssi"`
	SNR        *float64  `json:"snr"`
	Type       string    `json:"type,omitempty"`
	DestType   string    `json:"dest_type,omitempty"`
	Dest       string    `json:"dest,omitempty"`
	Hops       *int64    `json:"hops"`
	Via        string    `json:"via,omitempty"`
	Len        int64     `json:"len"`
	Frames     int64     `json:"frames,omitempty"`
	Partial    int64     `json:"partial,omitempty"`
	DelayMs    int64     `json:"delay_ms,omitempty"`
	Repeat     bool      `json:"repeat,omitempty"`
	Tx         string    `json:"tx,omitempty"`
	TxName     string    `json:"tx_name,omitempty"`
	DestName   string    `json:"dest_name,omitempty"`
	AppName    string    `json:"app_name,omitempty"`
	OrphanKind string    `json:"orphan_kind,omitempty"`

	// Proof rows: no observer heard the proved packet whole.
	ProvesUnheard bool `json:"proves_unheard,omitempty"`
	// Orphan rows: every observer that heard this same orphaned frame.
	GroupObservers []string `json:"group_observers,omitempty"`
	// Link between a partly heard transmission and the proof that it was
	// delivered (likely, see schema migration 3): on a proof row the orphan
	// group's id, on an orphan row the proof's packet id; and the time between.
	LinkID      int64 `json:"link_id,omitempty"`
	LinkDelayMs int64 `json:"link_delay_ms,omitempty"`
}

// Feed returns the newest receptions and unmatched orphans before a time,
// newest first.
func (s *Store) Feed(before time.Time, limit int) ([]FeedRow, error) {
	return s.feed("<", "DESC", before, limit)
}

// FeedAfter returns receptions and orphans after a time, oldest first, for
// the live view to catch up.
func (s *Store) FeedAfter(after time.Time, limit int) ([]FeedRow, error) {
	return s.feed(">", "ASC", after, limit)
}

func (s *Store) feed(cmp, order string, t0 time.Time, limit int) ([]FeedRow, error) {
	rows, err := s.DB.Query(`
		SELECT 'rx', r.id, p.id, r.t_rx, o.ext_id, r.rssi, r.snr, p.pkt_type, p.dest_type, p.dest_hash,
			r.hops, r.transport_id, p.data_len, r.frames, r.partial, r.delay_ms, p.repeat_of IS NOT NULL,
			r.tx_identity, d.display_name, d.app_name, NULL,
			COALESCE(p.proves_unheard, 0), NULL, p.proves_orphan,
			p.first_seen - (SELECT t FROM frame_errors WHERE id = p.proves_orphan), NULL
		FROM receptions r JOIN packets p ON p.id = r.packet_id JOIN observers o ON o.id = r.observer_id
		LEFT JOIN destinations d ON d.hash = p.dest_hash
		WHERE r.t_rx `+cmp+` ?
		UNION ALL
		SELECT 'orphan', f.id, NULL, f.t, o.ext_id, f.rssi, f.snr, NULL, NULL, NULL,
			NULL, NULL, f.len, 1, 0, 0, 0, NULL, NULL, NULL, f.kind,
			0, (SELECT group_concat(DISTINCT o2.ext_id) FROM frame_errors g JOIN observers o2 ON o2.id = g.observer_id
				WHERE COALESCE(g.group_id, g.id) = f.id),
			f.proof_packet_id, (SELECT first_seen FROM packets WHERE id = f.proof_packet_id) - f.t, f.data
		FROM frame_errors f JOIN observers o ON o.id = f.observer_id
		WHERE f.matched_packet_id IS NULL AND f.id = COALESCE(f.group_id, f.id) AND f.t `+cmp+` ?
		ORDER BY 4 `+order+`, 2 `+order+` LIMIT ?`, ms(t0), ms(t0), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedRow
	names := map[string]string{}
	var dests []int // orphan rows whose destination name is looked up below
	for rows.Next() {
		var (
			f                   FeedRow
			pid                 sql.NullInt64
			t                   int64
			ptype, dtype        sql.NullInt64
			dest, via, tx       []byte
			dname, aname, okind sql.NullString
			groupObs            sql.NullString
			linkID, linkDelay   sql.NullInt64
			head                []byte
		)
		if err := rows.Scan(&f.Kind, &f.ID, &pid, &t, &f.Observer, &f.RSSI, &f.SNR, &ptype, &dtype, &dest,
			&f.Hops, &via, &f.Len, &f.Frames, &f.Partial, &f.DelayMs, &f.Repeat, &tx, &dname, &aname, &okind,
			&f.ProvesUnheard, &groupObs, &linkID, &linkDelay, &head); err != nil {
			return nil, err
		}
		f.PacketID, f.T = pid.Int64, fromMs(t)
		if ptype.Valid {
			f.Type = reticulum.PacketType(ptype.Int64).String()
			f.DestType = reticulum.DestType(dtype.Int64).String()
		}
		f.Dest, f.Via, f.Tx = hx(dest), hx(via), hx(tx)
		f.DestName, f.AppName, f.OrphanKind = dname.String, aname.String, okind.String
		if f.AppName == "" && dest != nil {
			f.AppName = reticulum.WellKnownPlain(dest)
		}
		f.LinkID, f.LinkDelayMs = linkID.Int64, linkDelay.Int64
		if groupObs.Valid && groupObs.String != "" {
			f.GroupObservers = strings.Split(groupObs.String, ",")
		}
		// A first half carries the packet's header: show what it was.
		if f.Kind == "orphan" && len(head) > 0 {
			if hp, err := reticulum.Parse(head); err == nil {
				f.Type, f.DestType, f.Dest = hp.Type.String(), hp.DestType.String(), hx(hp.Dest)
				h := int64(hp.Hops)
				f.Hops = &h
				if hp.Header2 {
					f.Via = hx(hp.TransportID)
				}
				dests = append(dests, len(out))
			}
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // one connection: finish this query before the name lookups
	for _, i := range dests {
		b, _ := hex.DecodeString(out[i].Dest)
		var dn, an sql.NullString
		s.DB.QueryRow(`SELECT display_name, app_name FROM destinations WHERE hash = ?`, b).Scan(&dn, &an)
		out[i].DestName, out[i].AppName = dn.String, an.String
	}
	for i := range out {
		if tx := out[i].Tx; tx != "" {
			if _, ok := names[tx]; !ok {
				b, _ := hex.DecodeString(tx)
				names[tx] = s.identityName(b)
			}
			out[i].TxName = names[tx]
		}
	}
	return out, nil
}

// identityName picks the best display name among an identity's destinations.
func (s *Store) identityName(ident []byte) string {
	var name sql.NullString
	s.DB.QueryRow(`SELECT display_name FROM destinations WHERE identity_hash = ? AND display_name IS NOT NULL
		ORDER BY last_seen DESC LIMIT 1`, ident).Scan(&name)
	return name.String
}

// NodeRow is one radio node: an identity that announced destinations and/or
// was identified as the transmitter of packets.
type NodeRow struct {
	Identity     string    `json:"identity"`
	Name         string    `json:"name"`
	Apps         []string  `json:"apps"`
	Destinations int64     `json:"destinations"`
	Announces    int64     `json:"announces"`
	MinHops      *int64    `json:"min_hops"`
	LastSeen     time.Time `json:"last_seen"`
	HeardDirect  int64     `json:"heard_direct"` // receptions attributed to it
	LastRSSI     *float64  `json:"last_rssi"`
	LastSNR      *float64  `json:"last_snr"`
	AvgRSSI24h   *float64  `json:"avg_rssi_24h"`
	AvgSNR24h    *float64  `json:"avg_snr_24h"`
	// Distinct announces from this identity in the last 24 h (relayed copies
	// count once); the page shows it as a rate per hour
	Announces24h int64 `json:"announces_24h"`
	// Strongest reception of this node in the last 24 h, by any observer,
	// and which observer heard it
	BestRSSI24h     *float64 `json:"best_rssi_24h"`
	BestObserver24h string   `json:"best_observer_24h,omitempty"`
}

// Nodes lists nodes, optionally filtered by a search string matched against
// identity hash, destination hash, display name and app name.
func (s *Store) Nodes(q string, now time.Time) ([]NodeRow, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	day := ms(now.Add(-24 * time.Hour))
	rows, err := s.DB.Query(`
		WITH ids AS (
			SELECT identity_hash AS ident FROM destinations WHERE identity_hash IS NOT NULL
			UNION SELECT tx_identity FROM receptions WHERE tx_identity IS NOT NULL
		)
		SELECT ids.ident,
			(SELECT display_name FROM destinations WHERE identity_hash = ids.ident AND display_name IS NOT NULL ORDER BY last_seen DESC LIMIT 1),
			(SELECT group_concat(COALESCE(app_name, hex(name_hash)), '|') FROM destinations WHERE identity_hash = ids.ident),
			(SELECT COUNT(*) FROM destinations WHERE identity_hash = ids.ident),
			(SELECT COALESCE(SUM(announce_count),0) FROM destinations WHERE identity_hash = ids.ident),
			(SELECT MIN(last_hops) FROM destinations WHERE identity_hash = ids.ident),
			MAX(COALESCE((SELECT MAX(last_seen) FROM destinations WHERE identity_hash = ids.ident), 0),
			    COALESCE((SELECT MAX(t_rx) FROM receptions WHERE tx_identity = ids.ident), 0)),
			(SELECT COUNT(*) FROM receptions WHERE tx_identity = ids.ident),
			(SELECT rssi FROM receptions WHERE tx_identity = ids.ident ORDER BY t_rx DESC LIMIT 1),
			(SELECT snr FROM receptions WHERE tx_identity = ids.ident ORDER BY t_rx DESC LIMIT 1),
			(SELECT AVG(rssi) FROM receptions WHERE tx_identity = ids.ident AND t_rx >= ?),
			(SELECT AVG(snr) FROM receptions WHERE tx_identity = ids.ident AND t_rx >= ?),
			(SELECT COUNT(*) FROM announces WHERE identity_hash = ids.ident AND t_rx >= ?) AS ann24,
			(SELECT MAX(rssi) FROM receptions WHERE tx_identity = ids.ident AND t_rx >= ?),
			(SELECT o.ext_id FROM receptions r JOIN observers o ON o.id = r.observer_id
				WHERE r.tx_identity = ids.ident AND r.t_rx >= ? AND r.rssi IS NOT NULL ORDER BY r.rssi DESC LIMIT 1),
			(SELECT group_concat(lower(hex(hash)) || ' ' || lower(COALESCE(display_name,'')) || ' ' || lower(COALESCE(app_name,'')), ' ')
				FROM destinations WHERE identity_hash = ids.ident)
		FROM ids ORDER BY 7 DESC`, day, day, day, day, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeRow
	for rows.Next() {
		var (
			n          NodeRow
			ident      []byte
			name, apps sql.NullString
			search     sql.NullString
			bestObs    sql.NullString
			last       int64
		)
		if err := rows.Scan(&ident, &name, &apps, &n.Destinations, &n.Announces, &n.MinHops, &last,
			&n.HeardDirect, &n.LastRSSI, &n.LastSNR, &n.AvgRSSI24h, &n.AvgSNR24h, &n.Announces24h, &n.BestRSSI24h, &bestObs, &search); err != nil {
			return nil, err
		}
		n.Identity, n.Name, n.LastSeen = hx(ident), name.String, fromMs(last)
		n.BestObserver24h = bestObs.String
		if apps.String != "" {
			n.Apps = strings.Split(apps.String, "|")
		}
		if q != "" && !strings.Contains(n.Identity, q) && !strings.Contains(search.String, q) {
			continue
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodeDetail is everything the node page shows.
type NodeDetail struct {
	NodeRow
	Dests     []DestRow     `json:"dests"`
	Announces []AnnounceRow `json:"announce_history"`
	Signal    []SignalPoint `json:"signal"`
	Hourly    []HourRow     `json:"hourly"`
	Relayed   int64         `json:"relayed"` // announces from others it rebroadcast

	// Last 24 h, oldest hour first (24 entries): distinct announces per hour
	AnnounceRate []HourCount `json:"announce_rate"`
	// Last 24 h: how each observer hears this node
	PerObserver []ObserverSignal `json:"per_observer"`
	// Position set by hand (wins over announced positions on the map)
	Pin *Pin `json:"pin,omitempty"`
}

// HourCount is a count for one clock hour.
type HourCount struct {
	Hour  time.Time `json:"hour"`
	Count int64     `json:"count"`
}

// ObserverSignal summarises one observer's receptions of a node.
type ObserverSignal struct {
	Observer string    `json:"observer"`
	Local    bool      `json:"local"`
	LastRSSI *float64  `json:"last_rssi"`
	LastSNR  *float64  `json:"last_snr"`
	Count    int64     `json:"count"`
	RSSIAvg  float64   `json:"rssi_avg"`
	RSSIMin  float64   `json:"rssi_min"`
	RSSIMax  float64   `json:"rssi_max"`
	SNRAvg   float64   `json:"snr_avg"`
	SNRMin   float64   `json:"snr_min"`
	Last     time.Time `json:"last"`
}

type DestRow struct {
	Hash      string    `json:"hash"`
	App       string    `json:"app"`
	Name      string    `json:"name"`
	Announces int64     `json:"announces"`
	LastHops  *int64    `json:"last_hops"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Info      string    `json:"info,omitempty"`
	Lat       *float64  `json:"lat,omitempty"`
	Lon       *float64  `json:"lon,omitempty"`
	Height    *float64  `json:"height,omitempty"`
}

type AnnounceRow struct {
	T        time.Time `json:"t"`
	Dest     string    `json:"dest"`
	App      string    `json:"app"`
	Hops     int64     `json:"hops"`
	Valid    string    `json:"valid"`
	Copies   int64     `json:"copies"`    // receptions of this announce
	MaxDelay int64     `json:"max_delay"` // ms, latest relayed copy
}

type SignalPoint struct {
	T        time.Time `json:"t"`
	RSSI     float64   `json:"rssi"`
	SNR      float64   `json:"snr"`
	Type     string    `json:"type"`
	Observer string    `json:"observer"`
}

type HourRow struct {
	Hour      time.Time `json:"hour"`
	Observer  string    `json:"observer"`
	Count     int64     `json:"count"`
	RSSIAvg   float64   `json:"rssi_avg"`
	RSSIMin   float64   `json:"rssi_min"`
	RSSIMax   float64   `json:"rssi_max"`
	SNRAvg    float64   `json:"snr_avg"`
	Announces int64     `json:"announces"`
}

// Node returns one node's detail, or ErrNotFound.
func (s *Store) Node(identHex string, now time.Time) (*NodeDetail, error) {
	ident, err := hex.DecodeString(identHex)
	if err != nil || len(ident) != reticulum.HashLen {
		return nil, ErrNotFound
	}
	all, err := s.Nodes(identHex, now)
	if err != nil {
		return nil, err
	}
	var d NodeDetail
	found := false
	for _, n := range all {
		if n.Identity == identHex {
			d.NodeRow, found = n, true
		}
	}
	if !found {
		return nil, ErrNotFound
	}

	rows, err := s.DB.Query(`SELECT hash, COALESCE(app_name, hex(name_hash)), COALESCE(display_name,''),
		announce_count, last_hops, first_seen, last_seen, COALESCE(info, ''), lat, lon, height
		FROM destinations WHERE identity_hash = ? ORDER BY last_seen DESC`, ident)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r DestRow
		var h []byte
		var fs, ls int64
		if err := rows.Scan(&h, &r.App, &r.Name, &r.Announces, &r.LastHops, &fs, &ls, &r.Info, &r.Lat, &r.Lon, &r.Height); err != nil {
			rows.Close()
			return nil, err
		}
		r.Hash, r.FirstSeen, r.LastSeen = hx(h), fromMs(fs), fromMs(ls)
		d.Dests = append(d.Dests, r)
	}
	rows.Close()

	rows, err = s.DB.Query(`SELECT a.t_rx, a.dest_hash, COALESCE(dd.app_name, hex(a.name_hash)), a.hops, a.sig_valid,
		(SELECT COUNT(*) FROM receptions r WHERE r.packet_id = a.packet_id),
		(SELECT MAX(delay_ms) FROM receptions r WHERE r.packet_id = a.packet_id)
		FROM announces a LEFT JOIN destinations dd ON dd.hash = a.dest_hash
		WHERE a.identity_hash = ? ORDER BY a.t_rx DESC LIMIT 200`, ident)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r AnnounceRow
		var t int64
		var dh []byte
		var v int64
		if err := rows.Scan(&t, &dh, &r.App, &r.Hops, &v, &r.Copies, &r.MaxDelay); err != nil {
			rows.Close()
			return nil, err
		}
		r.T, r.Dest, r.Valid = fromMs(t), hx(dh), reticulum.Validity(v).String()
		d.Announces = append(d.Announces, r)
	}
	rows.Close()

	rows, err = s.DB.Query(`SELECT r.t_rx, r.rssi, r.snr, p.pkt_type, o.ext_id FROM receptions r
		JOIN packets p ON p.id = r.packet_id JOIN observers o ON o.id = r.observer_id
		WHERE r.tx_identity = ? AND r.rssi IS NOT NULL AND r.t_rx >= ? ORDER BY r.t_rx`,
		ident, ms(now.Add(-7*24*time.Hour)))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p SignalPoint
		var t, pt int64
		if err := rows.Scan(&t, &p.RSSI, &p.SNR, &pt, &p.Observer); err != nil {
			rows.Close()
			return nil, err
		}
		p.T, p.Type = fromMs(t), reticulum.PacketType(pt).String()
		d.Signal = append(d.Signal, p)
	}
	rows.Close()

	rows, err = s.DB.Query(`SELECT h.hour, o.ext_id, h.rx_count, h.rssi_sum / h.rx_count, h.rssi_min, h.rssi_max,
		h.snr_sum / h.rx_count, h.announces FROM node_stats_hourly h JOIN observers o ON o.id = h.observer_id
		WHERE h.tx_identity = ? ORDER BY h.hour DESC LIMIT 720`, ident)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r HourRow
		var hr int64
		if err := rows.Scan(&hr, &r.Observer, &r.Count, &r.RSSIAvg, &r.RSSIMin, &r.RSSIMax, &r.SNRAvg, &r.Announces); err != nil {
			rows.Close()
			return nil, err
		}
		r.Hour = fromMs(hr)
		d.Hourly = append(d.Hourly, r)
	}
	rows.Close()

	err = s.DB.QueryRow(`SELECT COUNT(*) FROM receptions r JOIN packets p ON p.id = r.packet_id
		WHERE r.tx_identity = ? AND r.header2 = 1 AND p.pkt_type = 1`, ident).Scan(&d.Relayed)
	if err != nil {
		return nil, err
	}

	if d.Pin, err = s.pinFor(ident); err != nil {
		return nil, err
	}

	// Announce rate: distinct announces per clock hour over the last 24 h
	first := now.Truncate(time.Hour).Add(-23 * time.Hour)
	counts := map[int64]int64{}
	rows, err = s.DB.Query(`SELECT t_rx FROM announces WHERE identity_hash = ? AND t_rx >= ?`, ident, ms(first))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		counts[fromMs(t).Truncate(time.Hour).UnixMilli()]++
	}
	rows.Close()
	for h := 0; h < 24; h++ {
		hr := first.Add(time.Duration(h) * time.Hour)
		d.AnnounceRate = append(d.AnnounceRate, HourCount{Hour: hr, Count: counts[hr.UnixMilli()]})
	}

	// Per observer, last 24 h
	rows, err = s.DB.Query(`SELECT o.ext_id, (o.source = 'rnode-serial' OR o.ext_id = ?3), COUNT(*), AVG(r.rssi), MIN(r.rssi), MAX(r.rssi),
			AVG(r.snr), MIN(r.snr), MAX(r.t_rx),
			(SELECT rssi FROM receptions x WHERE x.observer_id = o.id AND x.tx_identity = ?1 AND x.rssi IS NOT NULL ORDER BY x.t_rx DESC LIMIT 1),
			(SELECT snr FROM receptions x WHERE x.observer_id = o.id AND x.tx_identity = ?1 AND x.rssi IS NOT NULL ORDER BY x.t_rx DESC LIMIT 1)
		FROM receptions r JOIN observers o ON o.id = r.observer_id
		WHERE r.tx_identity = ?1 AND r.rssi IS NOT NULL AND r.t_rx >= ?2
		GROUP BY o.id ORDER BY (o.source = 'rnode-serial' OR o.ext_id = ?3) DESC, COUNT(*) DESC, o.ext_id`,
		ident, ms(now.Add(-24*time.Hour)), s.Setting("observer.local"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o ObserverSignal
		var last int64
		if err := rows.Scan(&o.Observer, &o.Local, &o.Count, &o.RSSIAvg, &o.RSSIMin, &o.RSSIMax, &o.SNRAvg, &o.SNRMin, &last,
			&o.LastRSSI, &o.LastSNR); err != nil {
			return nil, err
		}
		o.Last = fromMs(last)
		d.PerObserver = append(d.PerObserver, o)
	}
	return &d, rows.Err()
}

// ChannelPoint is one channel report.
type ChannelPoint struct {
	T            time.Time `json:"t"`
	UtilShort    float64   `json:"util_s"`
	UtilLong     float64   `json:"util_l"`
	AirtimeShort float64   `json:"airtime_s"`
	NoiseFloor   float64   `json:"noise_floor"`
	RSSI         float64   `json:"rssi"`
	Interference *float64  `json:"interference"`
}

// Channel returns an observer's channel reports since a time, thinned to at
// most max points by taking the busiest report in each bucket.
func (s *Store) Channel(observerID int64, since time.Time, max int) ([]ChannelPoint, error) {
	rows, err := s.DB.Query(`SELECT t, util_s, util_l, airtime_s, noise_floor, rssi, interference
		FROM channel_stats WHERE observer_id = ? AND t >= ? ORDER BY t`, observerID, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []ChannelPoint
	for rows.Next() {
		var p ChannelPoint
		var t int64
		if err := rows.Scan(&t, &p.UtilShort, &p.UtilLong, &p.AirtimeShort, &p.NoiseFloor, &p.RSSI, &p.Interference); err != nil {
			return nil, err
		}
		p.T = fromMs(t)
		all = append(all, p)
	}
	if err := rows.Err(); err != nil || len(all) <= max || max <= 0 {
		return all, err
	}
	step := float64(len(all)) / float64(max)
	out := make([]ChannelPoint, 0, max)
	for i := 0; i < max; i++ {
		lo, hi := int(float64(i)*step), int(float64(i+1)*step)
		best := all[lo]
		for _, p := range all[lo:hi] {
			if p.UtilShort > best.UtilShort {
				best = p
			}
		}
		out = append(out, best)
	}
	return out, nil
}

// ObserverView is one observer on the Observers page.
type ObserverView struct {
	ID        int64     `json:"id"`
	ExtID     string    `json:"ext_id"`
	Source    string    `json:"source"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// State: "online" (heard recently), "offline" (said so, e.g. its MQTT last
	// will), or "silent" (claims online but nothing heard for a while).
	State    string         `json:"state"`
	StatusT  *time.Time     `json:"status_t,omitempty"`
	Firmware string         `json:"firmware,omitempty"`
	Software string         `json:"software,omitempty"`
	Lat      *float64       `json:"lat,omitempty"`
	Lon      *float64       `json:"lon,omitempty"`
	Status   map[string]any `json:"status,omitempty"` // selected fields of its last status message

	Rx1h        int64    `json:"rx_1h"`
	Rx24h       int64    `json:"rx_24h"`
	Packets24h  int64    `json:"packets_24h"`
	OnlyHere24h int64    `json:"only_here_24h"` // packets no other observer heard
	Partial24h  int64    `json:"partial_24h"`
	Orphans24h  int64    `json:"orphans_24h"` // unmatched halves and unusable frames
	AvgRSSI1h   *float64 `json:"avg_rssi_1h"`
	AvgSNR1h    *float64 `json:"avg_snr_1h"`

	ChanT      *time.Time `json:"chan_t,omitempty"`
	UtilShort  *float64   `json:"util_s"`
	UtilLong   *float64   `json:"util_l"`
	NoiseFloor *float64   `json:"noise_floor"`

	Local bool `json:"local"`
	// Noise floor over the last 24 h in half-hour averages, oldest first
	// (missing half-hours omitted), for a sparkline
	Noise24h []SparkPoint `json:"noise_24h,omitempty"`
}

// SparkPoint is one value of a small time series.
type SparkPoint struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// SilentAfter is how long without data before an observer that claims to be
// online is shown as silent. Rrobs sends a status every 60 s.
const SilentAfter = 3 * time.Minute

// statusFields are the status-message fields shown on the page, when present.
var statusFields = []string{"uptime_s", "rx_frames", "rx_errors", "mqtt_dropped", "battery_mv", "rssi_wifi"}

// Observers lists every observer with its health figures, in a stable
// order: local first, then online, silent and offline, each by name.
func (s *Store) Observers(now time.Time) ([]ObserverView, error) {
	localExt := s.Setting("observer.local")
	hour, day := ms(now.Add(-time.Hour)), ms(now.Add(-24*time.Hour))
	rows, err := s.DB.Query(`
		SELECT o.id, o.ext_id, COALESCE(o.source,''), o.first_seen, o.last_seen, o.online, o.status_t,
			COALESCE(o.firmware,''), COALESCE(o.software,''), o.lat, o.lon, COALESCE(o.status_json,''),
			(SELECT COUNT(*) FROM receptions r WHERE r.observer_id = o.id AND r.t_rx >= ?1),
			(SELECT COUNT(*) FROM receptions r WHERE r.observer_id = o.id AND r.t_rx >= ?2),
			(SELECT COUNT(DISTINCT packet_id) FROM receptions r WHERE r.observer_id = o.id AND r.t_rx >= ?2),
			(SELECT COUNT(DISTINCT r.packet_id) FROM receptions r WHERE r.observer_id = o.id AND r.t_rx >= ?2
				AND NOT EXISTS (SELECT 1 FROM receptions x WHERE x.packet_id = r.packet_id AND x.observer_id <> o.id)),
			(SELECT COUNT(*) FROM receptions r WHERE r.observer_id = o.id AND r.t_rx >= ?2 AND r.partial > 0),
			(SELECT COUNT(*) FROM frame_errors f WHERE f.observer_id = o.id AND f.t >= ?2 AND f.matched_packet_id IS NULL),
			(SELECT AVG(rssi) FROM receptions r WHERE r.observer_id = o.id AND r.t_rx >= ?1),
			(SELECT AVG(snr) FROM receptions r WHERE r.observer_id = o.id AND r.t_rx >= ?1),
			c.t, c.util_s, c.util_l, c.noise_floor
		FROM observers o
		LEFT JOIN channel_stats c ON c.rowid = (SELECT rowid FROM channel_stats
			WHERE observer_id = o.id ORDER BY t DESC LIMIT 1)
		ORDER BY o.last_seen DESC`, hour, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObserverView
	for rows.Next() {
		var (
			v              ObserverView
			first, last    int64
			online         sql.NullBool
			statusT, chanT sql.NullInt64
			raw            string
		)
		if err := rows.Scan(&v.ID, &v.ExtID, &v.Source, &first, &last, &online, &statusT,
			&v.Firmware, &v.Software, &v.Lat, &v.Lon, &raw,
			&v.Rx1h, &v.Rx24h, &v.Packets24h, &v.OnlyHere24h, &v.Partial24h, &v.Orphans24h,
			&v.AvgRSSI1h, &v.AvgSNR1h, &chanT, &v.UtilShort, &v.UtilLong, &v.NoiseFloor); err != nil {
			return nil, err
		}
		v.FirstSeen, v.LastSeen = fromMs(first), fromMs(last)
		if statusT.Valid {
			t := fromMs(statusT.Int64)
			v.StatusT = &t
		}
		if chanT.Valid {
			t := fromMs(chanT.Int64)
			v.ChanT = &t
		}
		recent := now.Sub(v.LastSeen) < SilentAfter
		switch {
		case online.Valid && !online.Bool:
			v.State = "offline"
		case recent:
			v.State = "online"
		default:
			v.State = "silent"
		}
		if raw != "" {
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil {
				for _, k := range statusFields {
					if val, ok := m[k]; ok {
						if _, isNum := val.(float64); isNum {
							if v.Status == nil {
								v.Status = map[string]any{}
							}
							v.Status[k] = val
						}
					}
				}
			}
		}
		v.Local = isLocal(v.Source, v.ExtID, localExt)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	rank := map[string]int{"online": 0, "silent": 1, "offline": 2}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Local != b.Local {
			return a.Local
		}
		if rank[a.State] != rank[b.State] {
			return rank[a.State] < rank[b.State]
		}
		return a.ExtID < b.ExtID
	})
	for i := range out {
		spark, err := s.noiseSpark(out[i].ID, now)
		if err != nil {
			return nil, err
		}
		out[i].Noise24h = spark
	}
	return out, nil
}

// noiseSpark averages an observer's noise floor into half-hour buckets over
// the last 24 h.
func (s *Store) noiseSpark(observerID int64, now time.Time) ([]SparkPoint, error) {
	const bucket = 30 * 60 * 1000
	rows, err := s.DB.Query(`SELECT (t / ?1) * ?1, AVG(noise_floor) FROM channel_stats
		WHERE observer_id = ?2 AND t >= ?3 AND noise_floor IS NOT NULL
		GROUP BY t / ?1 ORDER BY 1`, bucket, observerID, ms(now.Add(-24*time.Hour)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SparkPoint
	for rows.Next() {
		var t int64
		var v float64
		if err := rows.Scan(&t, &v); err != nil {
			return nil, err
		}
		out = append(out, SparkPoint{T: fromMs(t), V: v})
	}
	return out, rows.Err()
}

// HeardNode is a node an observer heard transmit, directly.
type HeardNode struct {
	Identity string    `json:"identity"`
	Name     string    `json:"name"`
	Count    int64     `json:"count"`
	Last     time.Time `json:"last"`
	LastRSSI *float64  `json:"last_rssi"`
	LastSNR  *float64  `json:"last_snr"`
	AvgRSSI  *float64  `json:"avg_rssi"`
	AvgSNR   *float64  `json:"avg_snr"`
}

// ObserverNodes lists the nodes an observer heard directly since a time:
// receptions whose transmitter is known (an announce heard from its own
// node or from the relay that rebroadcast it, or a proof heard directly),
// most recently heard first.
func (s *Store) ObserverNodes(observerID int64, since time.Time) ([]HeardNode, error) {
	rows, err := s.DB.Query(`SELECT r.tx_identity, COUNT(*), MAX(r.t_rx), AVG(r.rssi), AVG(r.snr),
			(SELECT x.rssi FROM receptions x WHERE x.observer_id = ?1 AND x.tx_identity = r.tx_identity ORDER BY x.t_rx DESC LIMIT 1),
			(SELECT x.snr FROM receptions x WHERE x.observer_id = ?1 AND x.tx_identity = r.tx_identity ORDER BY x.t_rx DESC LIMIT 1)
		FROM receptions r
		WHERE r.observer_id = ?1 AND r.tx_identity IS NOT NULL AND r.t_rx >= ?2
		GROUP BY r.tx_identity ORDER BY MAX(r.t_rx) DESC`, observerID, ms(since))
	if err != nil {
		return nil, err
	}
	var out []HeardNode
	var idents [][]byte
	for rows.Next() {
		var n HeardNode
		var ident []byte
		var last int64
		if err := rows.Scan(&ident, &n.Count, &last, &n.AvgRSSI, &n.AvgSNR, &n.LastRSSI, &n.LastSNR); err != nil {
			rows.Close()
			return nil, err
		}
		n.Identity, n.Last = hx(ident), fromMs(last)
		out = append(out, n)
		idents = append(idents, ident)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // one connection: finish before the name lookups
	for i := range out {
		out[i].Name = s.identityName(idents[i])
	}
	return out, nil
}
