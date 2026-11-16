package store

import (
	"database/sql"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dkmr27/Rradmon/internal/reticulum"
)

// Packet-level live feed: one row per packet, shown at its first complete
// reception, which expands to every reception (other observers, relayed
// copies, halves heard alone). Unmatched orphan groups are rows of their own.

// PacketRow is one row of the packet feed.
type PacketRow struct {
	Kind     string    `json:"kind"` // "packet" or "orphan"
	ID       int64     `json:"id"`   // packet id, or the orphan group's id
	T        time.Time `json:"t"`    // first complete reception (orphans: first heard)
	LastSeen time.Time `json:"last_seen"`

	Type     string `json:"type,omitempty"`
	DestType string `json:"dest_type,omitempty"`
	Dest     string `json:"dest,omitempty"`
	DestName string `json:"dest_name,omitempty"`
	AppName  string `json:"app_name,omitempty"`
	Len      int64  `json:"len"`
	Hash     string `json:"hash,omitempty"`      // truncated (16 bytes), as used for dedupe
	FullHash string `json:"full_hash,omitempty"` // 32 bytes, as RNS shows packet hashes

	// The first observation: its observer, signal, hops and frames
	Observer string   `json:"observer"`
	FirstObs int64    `json:"first_obs"` // id of that observation (a reception, or for orphans the frame)
	RSSI     *float64 `json:"rssi"`
	SNR      *float64 `json:"snr"`
	Hops     *int64   `json:"hops"`
	Via      string   `json:"via,omitempty"`
	Frames   int64    `json:"frames"`
	Tx       string   `json:"tx,omitempty"`
	TxName   string   `json:"tx_name,omitempty"`

	// Everything else heard of it
	Receptions int64 `json:"receptions"`
	Observers  int64 `json:"observers"`
	Partials   int64 `json:"partials,omitempty"`
	MaxDelayMs int64 `json:"max_delay_ms,omitempty"`
	Repeat     bool  `json:"repeat,omitempty"`
	// Orphan rows absorbed into this packet (heard before the whole packet
	// arrived); the page removes them.
	Absorbed []int64 `json:"absorbed,omitempty"`

	ProvesUnheard bool  `json:"proves_unheard,omitempty"`
	LinkID        int64 `json:"link_id,omitempty"`
	LinkDelayMs   int64 `json:"link_delay_ms,omitempty"`

	// Orphan rows
	OrphanKind string `json:"orphan_kind,omitempty"`

	// Announce rows: what its app data says, in one line (name, interface,
	// radio settings, position known)
	Info           string   `json:"info,omitempty"`
	GroupObservers []string `json:"group_observers,omitempty"`
}

// PacketFeed lists packets and orphan groups. With after set it returns rows
// with any activity since then (new receptions update older rows), oldest
// first; otherwise rows first heard before `before`, newest first. hashPrefix
// (hex) narrows to packets whose full or truncated hash starts with it.
func (s *Store) PacketFeed(before, after time.Time, hashPrefix string, limit int) ([]PacketRow, error) {
	catchUp := !after.IsZero()
	var where, order string
	var args []any
	if catchUp {
		where, order = "p.last_seen > ?", "ASC"
		args = append(args, ms(after))
	} else {
		where, order = "p.first_seen < ?", "DESC"
		args = append(args, ms(before))
	}
	hashPrefix = strings.ToUpper(strings.TrimSpace(hashPrefix))
	if hashPrefix != "" {
		if _, err := hex.DecodeString(hashPrefix + strings.Repeat("0", len(hashPrefix)%2)); err != nil {
			return nil, nil // not hex: nothing matches
		}
		where += " AND (hex(p.full_hash) LIKE ? OR hex(p.hash) LIKE ?)"
		args = append(args, hashPrefix+"%", hashPrefix+"%")
	}
	args = append(args, limit)

	rows, err := s.DB.Query(`
		SELECT p.id, p.first_seen, p.last_seen, p.pkt_type, p.dest_type, p.dest_hash, p.data_len, p.hash, p.full_hash,
			p.repeat_of IS NOT NULL, COALESCE(p.proves_unheard, 0), p.proves_orphan,
			p.first_seen - (SELECT t FROM frame_errors WHERE id = p.proves_orphan),
			o.ext_id, r.id, r.rssi, r.snr, r.hops, r.transport_id, r.frames, COALESCE(r.tx_identity, p.tx_identity),
			(SELECT COUNT(*) FROM receptions WHERE packet_id = p.id),
			(SELECT COUNT(DISTINCT observer_id) FROM receptions WHERE packet_id = p.id),
			(SELECT COUNT(*) FROM receptions WHERE packet_id = p.id AND partial > 0),
			(SELECT MAX(delay_ms) FROM receptions WHERE packet_id = p.id),
			(SELECT group_concat(id) FROM frame_errors WHERE matched_packet_id = p.id),
			COALESCE(`+manualName("d.identity_hash")+`, d.display_name), d.app_name, a.name_hash, a.app_data
		FROM packets p
		JOIN receptions r ON r.id = (SELECT id FROM receptions WHERE packet_id = p.id
			ORDER BY partial > 0, t_rx, id LIMIT 1)
		JOIN observers o ON o.id = r.observer_id
		LEFT JOIN destinations d ON d.hash = p.dest_hash
		LEFT JOIN announces a ON a.packet_id = p.id
		WHERE `+where+` ORDER BY p.first_seen `+order+`, p.id `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	var out []PacketRow
	for rows.Next() {
		var (
			r                       PacketRow
			first, last             int64
			ptype, dtype            int64
			dest, hash, full        []byte
			via, tx                 []byte
			linkID, linkDelay, maxD sql.NullInt64
			absorbed                sql.NullString
			dname, aname            sql.NullString
			annNameHash, annData    []byte
		)
		if err := rows.Scan(&r.ID, &first, &last, &ptype, &dtype, &dest, &r.Len, &hash, &full,
			&r.Repeat, &r.ProvesUnheard, &linkID, &linkDelay,
			&r.Observer, &r.FirstObs, &r.RSSI, &r.SNR, &r.Hops, &via, &r.Frames, &tx,
			&r.Receptions, &r.Observers, &r.Partials, &maxD, &absorbed, &dname, &aname, &annNameHash, &annData); err != nil {
			rows.Close()
			return nil, err
		}
		r.Kind, r.T, r.LastSeen = "packet", fromMs(first), fromMs(last)
		r.Type, r.DestType = reticulum.PacketType(ptype).String(), reticulum.DestType(dtype).String()
		r.Dest, r.Hash, r.FullHash, r.Via, r.Tx = hx(dest), hx(hash), hx(full), hx(via), hx(tx)
		r.LinkID, r.LinkDelayMs, r.MaxDelayMs = linkID.Int64, linkDelay.Int64, maxD.Int64
		r.DestName, r.AppName = dname.String, aname.String
		if annNameHash != nil {
			r.Info = reticulum.DescribeAnnounce(annNameHash, annData).Summary
		}
		if r.AppName == "" {
			r.AppName = reticulum.WellKnownPlain(dest)
		}
		if absorbed.Valid {
			for _, f := range strings.Split(absorbed.String, ",") {
				if id, err := strconv.ParseInt(f, 10, 64); err == nil {
					r.Absorbed = append(r.Absorbed, id)
				}
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if hashPrefix == "" {
		orphans, err := s.orphanGroups(catchUp, before, after, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, orphans...)
		sort.SliceStable(out, func(i, j int) bool {
			if catchUp {
				return out[i].T.Before(out[j].T)
			}
			return out[i].T.After(out[j].T)
		})
		if len(out) > limit {
			out = out[:limit]
		}
	}
	s.nameRows(out)
	return out, nil
}

// orphanGroups lists unmatched orphan groups, one row per group.
func (s *Store) orphanGroups(catchUp bool, before, after time.Time, limit int) ([]PacketRow, error) {
	cond, order, t := "f.t < ?", "DESC", ms(before)
	if catchUp {
		// any member heard since: a late observer joining the group updates it
		cond, order, t = "(SELECT MAX(t) FROM frame_errors g WHERE COALESCE(g.group_id, g.id) = f.id) > ?", "ASC", ms(after)
	}
	rows, err := s.DB.Query(`
		SELECT f.id, f.t, (SELECT MAX(t) FROM frame_errors g WHERE COALESCE(g.group_id, g.id) = f.id),
			f.kind, f.len, f.rssi, f.snr, f.data, o.ext_id,
			(SELECT group_concat(DISTINCT o2.ext_id) FROM frame_errors g JOIN observers o2 ON o2.id = g.observer_id
				WHERE COALESCE(g.group_id, g.id) = f.id),
			(SELECT COUNT(*) FROM frame_errors g WHERE COALESCE(g.group_id, g.id) = f.id),
			f.proof_packet_id, (SELECT first_seen FROM packets WHERE id = f.proof_packet_id) - f.t
		FROM frame_errors f JOIN observers o ON o.id = f.observer_id
		WHERE f.matched_packet_id IS NULL AND f.id = COALESCE(f.group_id, f.id) AND `+cond+`
		ORDER BY f.t `+order+` LIMIT ?`, t, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PacketRow
	for rows.Next() {
		var (
			r                 PacketRow
			first, last       int64
			head              []byte
			group             sql.NullString
			linkID, linkDelay sql.NullInt64
		)
		if err := rows.Scan(&r.ID, &first, &last, &r.OrphanKind, &r.Len, &r.RSSI, &r.SNR, &head, &r.Observer,
			&group, &r.Receptions, &linkID, &linkDelay); err != nil {
			return nil, err
		}
		r.Kind, r.T, r.LastSeen, r.Frames, r.FirstObs = "orphan", fromMs(first), fromMs(last), 1, r.ID
		r.LinkID, r.LinkDelayMs = linkID.Int64, linkDelay.Int64
		if group.Valid && group.String != "" {
			r.GroupObservers = strings.Split(group.String, ",")
		}
		r.Observers = int64(len(r.GroupObservers))
		// A first half carries the packet's header
		if len(head) > 0 {
			if hp, err := reticulum.Parse(head); err == nil {
				r.Type, r.DestType, r.Dest = hp.Type.String(), hp.DestType.String(), hx(hp.Dest)
				h := int64(hp.Hops)
				r.Hops = &h
				if hp.Header2 {
					r.Via = hx(hp.TransportID)
				}
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// nameRows fills transmitter names, and destination names for orphan rows.
func (s *Store) nameRows(out []PacketRow) {
	names := map[string]string{}
	for i := range out {
		if tx := out[i].Tx; tx != "" {
			if _, ok := names[tx]; !ok {
				b, _ := hex.DecodeString(tx)
				names[tx] = s.identityName(b)
			}
			out[i].TxName = names[tx]
		}
		if out[i].Kind == "orphan" && out[i].Dest != "" {
			b, _ := hex.DecodeString(out[i].Dest)
			var dn, an sql.NullString
			s.DB.QueryRow(`SELECT COALESCE(`+manualName("identity_hash")+`, display_name), app_name
				FROM destinations WHERE hash = ?`, b).Scan(&dn, &an)
			out[i].DestName, out[i].AppName = dn.String, an.String
		}
	}
}

// Observation is one reception (or orphaned frame) under an expanded row.
type Observation struct {
	ID       int64     `json:"id"` // reception id, or frame id for orphans
	Observer string    `json:"observer"`
	T        time.Time `json:"t"`
	DelayMs  int64     `json:"delay_ms"` // after the row's first complete reception
	RSSI     *float64  `json:"rssi"`
	SNR      *float64  `json:"snr"`
	Hops     *int64    `json:"hops"`
	Via      string    `json:"via,omitempty"`
	// Frames: "S" one LoRa frame, "D" both frames of a split packet, "O" one
	// half of a split packet heard alone
	Frames string `json:"frames"`
	Part   string `json:"part,omitempty"` // for O: "first half" or "second half"
	Tx     string `json:"tx,omitempty"`
	TxName string `json:"tx_name,omitempty"`
}

// PacketObservations lists every reception of a packet, oldest first.
func (s *Store) PacketObservations(packetID int64) ([]Observation, error) {
	rows, err := s.DB.Query(`SELECT r.id, o.ext_id, r.t_rx, r.delay_ms, r.rssi, r.snr, r.hops, r.transport_id,
		r.frames, r.partial, r.tx_identity
		FROM receptions r JOIN observers o ON o.id = r.observer_id
		WHERE r.packet_id = ? ORDER BY r.t_rx, r.id`, packetID)
	if err != nil {
		return nil, err
	}
	var out []Observation
	for rows.Next() {
		var (
			ob           Observation
			t            int64
			frames, part int64
			via, tx      []byte
		)
		if err := rows.Scan(&ob.ID, &ob.Observer, &t, &ob.DelayMs, &ob.RSSI, &ob.SNR, &ob.Hops, &via, &frames, &part, &tx); err != nil {
			rows.Close()
			return nil, err
		}
		ob.T, ob.Via, ob.Tx = fromMs(t), hx(via), hx(tx)
		switch {
		case part > 0:
			ob.Frames, ob.Part = "O", map[int64]string{1: "first half", 2: "second half"}[part]
		case frames == 2:
			ob.Frames = "D"
		default:
			ob.Frames = "S"
		}
		out = append(out, ob)
	}
	rows.Close()
	names := map[string]string{}
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

// OrphanObservations lists every observer's copy of an orphan group.
func (s *Store) OrphanObservations(groupID int64) ([]Observation, error) {
	rows, err := s.DB.Query(`SELECT f.id, o.ext_id, f.t, f.t - (SELECT t FROM frame_errors WHERE id = ?), f.rssi, f.snr, f.kind
		FROM frame_errors f JOIN observers o ON o.id = f.observer_id
		WHERE COALESCE(f.group_id, f.id) = ? ORDER BY f.t, f.id`, groupID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var ob Observation
		var t int64
		if err := rows.Scan(&ob.ID, &ob.Observer, &t, &ob.DelayMs, &ob.RSSI, &ob.SNR, &ob.Part); err != nil {
			return nil, err
		}
		ob.T, ob.Frames = fromMs(t), "O"
		out = append(out, ob)
	}
	return out, rows.Err()
}
