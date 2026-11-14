// Package store is Rradmon's SQLite database. See docs/spec-v0.md §7.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dkmr27/Rradmon/internal/reticulum"
)

// Store wraps the database. It is safe for concurrent use; writes are
// serialised on one connection.
type Store struct {
	DB *sql.DB
}

// Open opens or creates the database at path and brings the schema up to date.
// Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; also keeps ":memory:" a single database
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.backfillFullHash(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.backfillAnnounceInfo(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// backfillFullHash computes the full packet hash for packets stored before
// it was kept, from their stored bytes (packets without payload stay unset).
func (s *Store) backfillFullHash() error {
	for {
		rows, err := s.DB.Query(`SELECT id, payload FROM packets
			WHERE full_hash IS NULL AND payload IS NOT NULL LIMIT 500`)
		if err != nil {
			return err
		}
		type item struct {
			id   int64
			hash []byte
		}
		var items []item
		for rows.Next() {
			var id int64
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			h := []byte{} // unparseable: mark done with an empty value
			if p, err := reticulum.Parse(raw); err == nil {
				full := p.Hash()
				h = full[:]
			}
			items = append(items, item{id, h})
		}
		rows.Close()
		if len(items) == 0 {
			return nil
		}
		for _, it := range items {
			if _, err := s.DB.Exec(`UPDATE packets SET full_hash = ? WHERE id = ?`, it.hash, it.id); err != nil {
				return err
			}
		}
	}
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// Network returns the id of the radio profile, creating it if needed.
func (s *Store) Network(slug string, freqHz, bwHz uint32, sf, cr uint8) (int64, error) {
	_, err := s.DB.Exec(`INSERT INTO networks (slug, freq_hz, bw_hz, sf, cr) VALUES (?,?,?,?,?)
		ON CONFLICT(slug) DO NOTHING`, slug, freqHz, bwHz, sf, cr)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.DB.QueryRow(`SELECT id FROM networks WHERE slug = ?`, slug).Scan(&id)
	return id, err
}

// Observer returns the id of an observer, creating it and updating last_seen.
func (s *Store) Observer(extID, source string, t time.Time) (int64, error) {
	_, err := s.DB.Exec(`INSERT INTO observers (ext_id, name, source, first_seen, last_seen) VALUES (?,?,?,?,?)
		ON CONFLICT(ext_id) DO UPDATE SET last_seen = MAX(last_seen, excluded.last_seen)`,
		extID, extID, source, ms(t), ms(t))
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.DB.QueryRow(`SELECT id FROM observers WHERE ext_id = ?`, extID).Scan(&id)
	return id, err
}

// ObserverState is what an observer reports about itself.
type ObserverState struct {
	Online   bool
	Firmware string
	Software string
	Lat, Lon *float64
	// Raw is the status message as received (JSON), kept for fields the
	// UI shows when present (uptime, counters, battery). Empty keeps the old.
	Raw string
}

// ObserverStatus records an observer's reported status. An offline report
// (an MQTT last will) does not move last_seen, which stays the last time the
// observer itself was heard.
func (s *Store) ObserverStatus(extID, source string, t time.Time, st ObserverState) error {
	if st.Lat == nil || st.Lon == nil || !ValidPos(*st.Lat, *st.Lon) {
		st.Lat, st.Lon = nil, nil // 0,0 or out of range: no position
	}
	// An online status creates the observer if new. An offline one only
	// updates a known observer: the broker keeps the last will of every
	// observer that ever connected, and a server that never saw one should
	// not list it. Neither touches last_seen of a known observer when
	// offline: a retained status from yesterday must not make it look seen now.
	if st.Online {
		if _, err := s.DB.Exec(`INSERT INTO observers (ext_id, name, source, first_seen, last_seen)
			VALUES (?,?,?,?,?) ON CONFLICT(ext_id) DO NOTHING`, extID, extID, source, ms(t), ms(t)); err != nil {
			return err
		}
		if err := s.TouchObserver(extID, t); err != nil {
			return err
		}
	}
	_, err := s.DB.Exec(`UPDATE observers SET online = ?, status_t = ?,
		firmware = COALESCE(NULLIF(?, ''), firmware),
		software = COALESCE(NULLIF(?, ''), software),
		status_json = COALESCE(NULLIF(?, ''), status_json),
		lat = COALESCE(?, lat), lon = COALESCE(?, lon)
		WHERE ext_id = ?`,
		st.Online, ms(t), st.Firmware, st.Software, st.Raw, st.Lat, st.Lon, extID)
	return err
}

// TouchObserver moves an observer's last_seen forward.
func (s *Store) TouchObserver(extID string, t time.Time) error {
	_, err := s.DB.Exec(`UPDATE observers SET last_seen = MAX(last_seen, ?) WHERE ext_id = ?`, ms(t), extID)
	return err
}

// Settings with their defaults (spec §7).
var defaultSettings = map[string]string{
	"ingest.store_payloads": "1",
	// An MQTT observer to treat as local ("us"); RNodes attached to this
	// server are always local
	"observer.local":         "",
	"ingest.window_announce": "4h",
	"ingest.window_other":    "10m",
	"retain.receptions_days": "14",
	"retain.announces_days":  "60",
	"retain.channel_days":    "30",
	"retain.node_stats_days": "30",
}

// Setting returns a setting's value or its default.
func (s *Store) Setting(key string) string {
	var v string
	if err := s.DB.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err == nil {
		return v
	}
	return defaultSettings[key]
}

// SetSetting stores a setting.
func (s *Store) SetSetting(key, value string) error {
	if _, ok := defaultSettings[key]; !ok {
		return fmt.Errorf("store: unknown setting %q", key)
	}
	_, err := s.DB.Exec(`INSERT INTO settings (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SettingInt, SettingBool and SettingDuration parse a setting, falling back
// to the default if the stored value is malformed.
func (s *Store) SettingInt(key string) int {
	if n, err := strconv.Atoi(s.Setting(key)); err == nil {
		return n
	}
	n, _ := strconv.Atoi(defaultSettings[key])
	return n
}

func (s *Store) SettingBool(key string) bool {
	v := s.Setting(key)
	return v == "1" || v == "true" || v == "yes"
}

func (s *Store) SettingDuration(key string) time.Duration {
	if d, err := time.ParseDuration(s.Setting(key)); err == nil {
		return d
	}
	d, _ := time.ParseDuration(defaultSettings[key])
	return d
}

// ChannelStats is one channel report from an observer.
type ChannelStats struct {
	T                                              time.Time
	AirtimeShort, AirtimeLong, UtilShort, UtilLong float32
	RSSI, NoiseFloor                               float32
	Interference                                   *float32
}

// AddChannelStats stores a channel report.
func (s *Store) AddChannelStats(observerID int64, c ChannelStats) error {
	_, err := s.DB.Exec(`INSERT INTO channel_stats
		(observer_id, t, airtime_s, airtime_l, util_s, util_l, rssi, noise_floor, interference)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		observerID, ms(c.T), c.AirtimeShort, c.AirtimeLong, c.UtilShort, c.UtilLong,
		c.RSSI, c.NoiseFloor, c.Interference)
	return err
}

// PurgeResult counts deleted rows.
type PurgeResult struct {
	Receptions, Packets, Announces, Channel, FrameErrors, NodeStats, Destinations int64
}

// Purge deletes data older than the retention settings, in batches so the
// writer is never held for long.
func (s *Store) Purge(ctx context.Context, now time.Time) (PurgeResult, error) {
	var r PurgeResult
	day := 24 * time.Hour
	cut := func(key string) int64 { return ms(now.Add(-time.Duration(s.SettingInt(key)) * day)) }
	recCut, annCut := cut("retain.receptions_days"), cut("retain.announces_days")
	chanCut, nodeCut := cut("retain.channel_days"), cut("retain.node_stats_days")

	batch := func(q string, args ...any) (int64, error) {
		var total int64
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			res, err := s.DB.ExecContext(ctx, q, args...)
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += n
			if n < 5000 {
				return total, nil
			}
		}
	}
	var err error
	steps := []struct {
		n *int64
		q string
		a []any
	}{
		{&r.Receptions, `DELETE FROM receptions WHERE rowid IN (SELECT rowid FROM receptions WHERE t_rx < ? LIMIT 5000)`, []any{recCut}},
		{&r.FrameErrors, `DELETE FROM frame_errors WHERE rowid IN (SELECT rowid FROM frame_errors WHERE t < ? LIMIT 5000)`, []any{recCut}},
		{&r.Channel, `DELETE FROM channel_stats WHERE rowid IN (SELECT rowid FROM channel_stats WHERE t < ? LIMIT 5000)`, []any{chanCut}},
		{&r.NodeStats, `DELETE FROM node_stats_hourly WHERE rowid IN (SELECT rowid FROM node_stats_hourly WHERE hour < ? LIMIT 5000)`, []any{nodeCut}},
		// Announces outlive receptions: keep their packets until the announce goes.
		{&r.Announces, `DELETE FROM announces WHERE rowid IN (SELECT rowid FROM announces WHERE t_rx < ? LIMIT 5000)`, []any{annCut}},
		{&r.Packets, `DELETE FROM packets WHERE rowid IN (SELECT p.rowid FROM packets p
			WHERE p.last_seen < ?
			AND NOT EXISTS (SELECT 1 FROM receptions r WHERE r.packet_id = p.id)
			AND NOT EXISTS (SELECT 1 FROM announces a WHERE a.packet_id = p.id) LIMIT 5000)`, []any{recCut}},
		{&r.Destinations, `DELETE FROM destinations WHERE rowid IN (SELECT rowid FROM destinations WHERE last_seen < ? LIMIT 5000)`, []any{annCut}},
	}
	for _, st := range steps {
		if *st.n, err = batch(st.q, st.a...); err != nil {
			return r, err
		}
	}
	return r, nil
}

var ErrNotFound = errors.New("store: not found")
