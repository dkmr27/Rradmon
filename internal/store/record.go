package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/dkmr27/Rradmon/internal/reticulum"
)

// Reception is one hearing of a whole Reticulum packet by one observer.
type Reception struct {
	NetworkID  int64
	ObserverID int64
	T          time.Time  // receive time (orders events)
	TObs       *time.Time // observer's own timestamp, if it sent one
	RSSI, SNR  *float32
	Packet     *reticulum.Packet
	Announce   *reticulum.Announce // set for announces
	Frames     int                 // 1, or 2 for a rejoined split packet
	FrameHash  [][]byte            // per-frame hashes, split packets only

	StorePayload   bool
	WindowAnnounce time.Duration
	WindowOther    time.Duration
}

// Recorded says what RecordReception did.
type Recorded struct {
	PacketID    int64
	ReceptionID int64
	New         bool  // first reception of this packet within the window
	RepeatOf    int64 // older packet with the same hash, outside the window
	Delay       time.Duration
	TxIdentity  []byte // who transmitted this copy, when it can be inferred
	Partials    int    // earlier orphans matched to this packet
}

// RecordReception stores a reception, deduplicating by packet hash within the
// window for its type (spec §4), and attributes the transmitter where the
// packet allows it.
func (s *Store) RecordReception(r Reception) (Recorded, error) {
	var out Recorded
	tx, err := s.DB.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	p := r.Packet
	hash := p.TruncatedHash()
	window := r.WindowOther
	if p.Type == reticulum.TypeAnnounce {
		window = r.WindowAnnounce
	}
	t := ms(r.T)

	var firstSeen int64
	err = tx.QueryRow(`SELECT id, first_seen FROM packets WHERE network_id = ? AND hash = ?
		ORDER BY first_seen DESC LIMIT 1`, r.NetworkID, hash).Scan(&out.PacketID, &firstSeen)
	switch {
	case err == nil && t-firstSeen <= window.Milliseconds():
		out.Delay = time.Duration(t-firstSeen) * time.Millisecond
		if _, err := tx.Exec(`UPDATE packets SET last_seen = MAX(last_seen, ?), rx_count = rx_count + 1 WHERE id = ?`,
			t, out.PacketID); err != nil {
			return out, err
		}
	case err == nil || errors.Is(err, sql.ErrNoRows):
		if err == nil {
			out.RepeatOf = out.PacketID
		}
		out.New = true
		var payload []byte
		if r.StorePayload || p.Type == reticulum.TypeAnnounce {
			payload = p.Raw
		}
		var h1, h2 []byte
		if len(r.FrameHash) == 2 {
			h1, h2 = r.FrameHash[0], r.FrameHash[1]
		}
		res, err := tx.Exec(`INSERT INTO packets (network_id, hash, first_seen, last_seen, pkt_type, dest_type,
			context, dest_hash, data_len, rx_count, payload, h1, h2, repeat_of)
			VALUES (?,?,?,?,?,?,?,?,?,1,?,?,?,?)`,
			r.NetworkID, hash, t, t, p.Type, p.DestType, p.Context, p.Dest, len(p.Data),
			payload, h1, h2, nullID(out.RepeatOf))
		if err != nil {
			return out, err
		}
		out.PacketID, _ = res.LastInsertId()
	default:
		return out, err
	}

	out.TxIdentity, err = attribute(tx, r, p)
	if err != nil {
		return out, err
	}
	if out.New && out.TxIdentity != nil {
		if _, err := tx.Exec(`UPDATE packets SET tx_identity = ? WHERE id = ?`, out.TxIdentity, out.PacketID); err != nil {
			return out, err
		}
	}

	var tObs any
	if r.TObs != nil {
		tObs = ms(*r.TObs)
	}
	res, err := tx.Exec(`INSERT INTO receptions (packet_id, observer_id, t_rx, t_obs, rssi, snr, hops, header2,
		transport, transport_id, frames, partial, delay_ms, tx_identity)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,0,?,?)`,
		out.PacketID, r.ObserverID, t, tObs, f32(r.RSSI), f32(r.SNR), p.Hops, p.Header2, p.Transport,
		nullBytes(p.TransportID), r.Frames, out.Delay.Milliseconds(), nullBytes(out.TxIdentity))
	if err != nil {
		return out, err
	}
	out.ReceptionID, _ = res.LastInsertId()

	if r.Announce != nil {
		if err := recordAnnounce(tx, r, out); err != nil {
			return out, err
		}
	}
	if out.TxIdentity != nil && r.RSSI != nil && r.SNR != nil {
		if err := nodeStats(tx, out.TxIdentity, r.ObserverID, r.T, *r.RSSI, *r.SNR, p.Type == reticulum.TypeAnnounce); err != nil {
			return out, err
		}
	}
	if out.New && len(r.FrameHash) == 2 {
		if out.Partials, err = matchEarlierOrphans(tx, r, out.PacketID, window); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

// attribute infers which node transmitted this copy of the packet:
//   - an announce heard directly (hops 0, header 1) comes from its identity;
//   - a rebroadcast announce (header 2) comes from the relay in transport_id;
//   - a PROOF heard directly comes from whoever owns the destination of the
//     packet it proves (a probe, a link request), if that identity is known.
func attribute(tx *sql.Tx, r Reception, p *reticulum.Packet) ([]byte, error) {
	switch p.Type {
	case reticulum.TypeAnnounce:
		if p.Header2 {
			return p.TransportID, nil
		}
		if p.Hops == 0 && r.Announce != nil {
			return r.Announce.IdentityHash, nil
		}
	case reticulum.TypeProof:
		if p.Hops != 0 {
			return nil, nil
		}
		var ident []byte
		err := tx.QueryRow(`SELECT d.identity_hash FROM packets pk
			JOIN destinations d ON d.hash = pk.dest_hash
			WHERE pk.network_id = ? AND pk.hash = ? AND pk.first_seen >= ?
			ORDER BY pk.first_seen DESC LIMIT 1`,
			r.NetworkID, p.Dest, ms(r.T.Add(-r.WindowOther))).Scan(&ident)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return ident, err
	}
	return nil, nil
}

func recordAnnounce(tx *sql.Tx, r Reception, out Recorded) error {
	a := r.Announce
	t := ms(r.T)
	if out.New {
		var tAnn any
		if !a.Emitted.IsZero() {
			tAnn = ms(a.Emitted)
		}
		if _, err := tx.Exec(`INSERT INTO announces (packet_id, dest_hash, identity_hash, public_key, name_hash,
			random_hash, ratchet, app_data, t_rx, hops, sig_valid, t_announced) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			out.PacketID, a.Dest, a.IdentityHash, a.PublicKey, a.NameHash, a.RandomHash,
			nullBytes(a.Ratchet), nullBytes(a.AppData), t, r.Packet.Hops, a.Validity, tAnn); err != nil {
			return err
		}
	}
	// Invalid announces are kept but never name or update a destination.
	if a.Validity != reticulum.Valid {
		return nil
	}
	count := 0
	if out.New {
		count = 1
	}
	appName := reticulum.WellKnownName(a.NameHash)
	display := reticulum.DisplayName(a.NameHash, a.AppData)
	_, err := tx.Exec(`INSERT INTO destinations (hash, identity_hash, name_hash, app_name, display_name,
		first_seen, last_seen, announce_count, last_hops) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(hash) DO UPDATE SET
			last_seen = MAX(last_seen, excluded.last_seen),
			announce_count = announce_count + excluded.announce_count,
			display_name = COALESCE(excluded.display_name, display_name),
			app_name = COALESCE(excluded.app_name, app_name),
			last_hops = CASE WHEN ? THEN excluded.last_hops ELSE MIN(last_hops, excluded.last_hops) END`,
		a.Dest, a.IdentityHash, a.NameHash, nullStr(appName), nullStr(display), t, t, count, r.Packet.Hops, out.New)
	return err
}

func nodeStats(tx *sql.Tx, ident []byte, obs int64, t time.Time, rssi, snr float32, announce bool) error {
	hour := t.Truncate(time.Hour).UnixMilli()
	ann := 0
	if announce {
		ann = 1
	}
	_, err := tx.Exec(`INSERT INTO node_stats_hourly (tx_identity, observer_id, hour, rx_count,
		rssi_sum, rssi_min, rssi_max, snr_sum, snr_min, snr_max, announces) VALUES (?,?,?,1,?,?,?,?,?,?,?)
		ON CONFLICT(tx_identity, observer_id, hour) DO UPDATE SET
			rx_count = rx_count + 1,
			rssi_sum = rssi_sum + excluded.rssi_sum, rssi_min = MIN(rssi_min, excluded.rssi_min), rssi_max = MAX(rssi_max, excluded.rssi_max),
			snr_sum = snr_sum + excluded.snr_sum, snr_min = MIN(snr_min, excluded.snr_min), snr_max = MAX(snr_max, excluded.snr_max),
			announces = announces + excluded.announces`,
		ident, obs, hour, rssi, rssi, rssi, snr, snr, snr, ann)
	return err
}

// Orphan is a half of a split packet heard without its other half, or a
// frame that could not be used.
type Orphan struct {
	NetworkID  int64
	ObserverID int64
	T          time.Time
	RSSI, SNR  *float32
	Kind       string // "first half", "second half", "short frame", "unparsed: ..."
	Header     *byte  // RNode header byte, when known
	Len        int
	FrameHash  []byte // for halves: matched against packets.h1/h2
	Head       []byte // first bytes of a first half (its Reticulum header)
	Window     time.Duration
}

// RecordOrphan stores an orphan and, if it is a half of a split packet already
// seen whole, also records it as a partial reception of that packet.
// It returns the matched packet id, or 0.
func (s *Store) RecordOrphan(o Orphan) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var hdr any
	if o.Header != nil {
		hdr = *o.Header
	}
	res, err := tx.Exec(`INSERT INTO frame_errors (observer_id, t, kind, rssi, snr, len, header, frame_hash, data)
		VALUES (?,?,?,?,?,?,?,?,?)`, o.ObserverID, ms(o.T), o.Kind, f32(o.RSSI), f32(o.SNR), o.Len, hdr,
		nullBytes(o.FrameHash), nullBytes(o.Head))
	if err != nil {
		return 0, err
	}
	feID, _ := res.LastInsertId()
	var pid int64
	if o.FrameHash != nil {
		var part int
		err := tx.QueryRow(`SELECT id, CASE WHEN h1 = ? THEN 1 ELSE 2 END FROM packets
			WHERE network_id = ? AND (h1 = ? OR h2 = ?) AND first_seen >= ? AND first_seen <= ?
			ORDER BY first_seen DESC LIMIT 1`,
			o.FrameHash, o.NetworkID, o.FrameHash, o.FrameHash, ms(o.T.Add(-o.Window)), ms(o.T.Add(o.Window))).Scan(&pid, &part)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if pid != 0 {
			if err := addPartial(tx, pid, feID, o.ObserverID, ms(o.T), o.RSSI, o.SNR, part, o.Head); err != nil {
				return 0, err
			}
		}
	}
	return pid, tx.Commit()
}

// matchEarlierOrphans links orphans heard before the whole packet arrived.
func matchEarlierOrphans(tx *sql.Tx, r Reception, pid int64, window time.Duration) (int, error) {
	rows, err := tx.Query(`SELECT id, observer_id, t, rssi, snr, frame_hash, data FROM frame_errors
		WHERE matched_packet_id IS NULL AND frame_hash IN (?, ?) AND t >= ?`,
		r.FrameHash[0], r.FrameHash[1], ms(r.T.Add(-window)))
	if err != nil {
		return 0, err
	}
	type orphan struct {
		id, obs, t int64
		rssi, snr  sql.NullFloat64
		fh, head   []byte
	}
	var found []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.obs, &o.t, &o.rssi, &o.snr, &o.fh, &o.head); err != nil {
			rows.Close()
			return 0, err
		}
		found = append(found, o)
	}
	rows.Close()
	for _, o := range found {
		part := 2
		if string(o.fh) == string(r.FrameHash[0]) {
			part = 1
		}
		if err := addPartial(tx, pid, o.id, o.obs, o.t, nf(o.rssi), nf(o.snr), part, o.head); err != nil {
			return 0, err
		}
	}
	return len(found), nil
}

func addPartial(tx *sql.Tx, pid, frameErrorID, obs, t int64, rssi, snr *float32, part int, head []byte) error {
	var hops, header2, transport any
	if part == 1 && len(head) >= 2 {
		hops, header2, transport = head[1], head[0]&0x40 != 0, head[0]&0x10 != 0
	}
	var firstSeen int64
	if err := tx.QueryRow(`SELECT first_seen FROM packets WHERE id = ?`, pid).Scan(&firstSeen); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO receptions (packet_id, observer_id, t_rx, rssi, snr, hops, header2,
		transport, frames, partial, delay_ms) VALUES (?,?,?,?,?,?,?,?,1,?,?)`,
		pid, obs, t, f32(rssi), f32(snr), hops, header2, transport, part, t-firstSeen); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE frame_errors SET matched_packet_id = ? WHERE id = ?`, pid, frameErrorID)
	return err
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullBytes(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func f32(p *float32) any {
	if p == nil {
		return nil
	}
	return float64(*p)
}

func nf(n sql.NullFloat64) *float32 {
	if !n.Valid {
		return nil
	}
	v := float32(n.Float64)
	return &v
}
