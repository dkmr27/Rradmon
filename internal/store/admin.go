package store

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Admin-only state: the admin password hash, editable settings with
// validation, manual node positions (pins), and maintenance actions.

const adminHashKey = "admin.password_hash"

// AdminPasswordHash returns the stored bcrypt hash, or "" if none is set.
func (s *Store) AdminPasswordHash() string {
	var v string
	if err := s.DB.QueryRow(`SELECT value FROM settings WHERE key = ?`, adminHashKey).Scan(&v); err != nil {
		return ""
	}
	return v
}

// SetAdminPasswordHash stores a bcrypt hash ("" removes the password, which
// disables admin login).
func (s *Store) SetAdminPasswordHash(hash string) error {
	if hash == "" {
		_, err := s.DB.Exec(`DELETE FROM settings WHERE key = ?`, adminHashKey)
		return err
	}
	_, err := s.DB.Exec(`INSERT INTO settings (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, adminHashKey, hash)
	return err
}

// SettingInfo describes an editable setting for the admin page.
type SettingInfo struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Help    string `json:"help"`
	Kind    string `json:"kind"` // "days", "bool", "duration", "observer"
	Value   string `json:"value"`
	Default string `json:"default"`
}

var settingInfo = []SettingInfo{
	{Key: "observer.local", Label: "Local observer", Kind: "observer",
		Help: "An MQTT observer to treat as local (\"us\"). An RNode attached to this server is always local."},
	{Key: "ingest.store_payloads", Label: "Store payloads", Kind: "bool",
		Help: "Keep the bytes of non-announce packets. Off stores only their metadata; announces are always kept."},
	{Key: "retain.receptions_days", Label: "Keep receptions (days)", Kind: "days",
		Help: "Receptions, packets and frame errors older than this are deleted."},
	{Key: "retain.announces_days", Label: "Keep announces (days)", Kind: "days",
		Help: "Announces and destinations not heard for this long are deleted."},
	{Key: "retain.channel_days", Label: "Keep channel stats (days)", Kind: "days",
		Help: "Noise floor and utilisation reports."},
	{Key: "retain.node_stats_days", Label: "Keep hourly node stats (days)", Kind: "days",
		Help: "Hourly RSSI/SNR summaries per node and observer."},
	{Key: "retain.observers_days", Label: "Keep silent observers (days)", Kind: "days",
		Help: "Observers not heard for this long, once none of their receptions are left, are removed. One heard again comes back."},
	{Key: "ingest.window_other", Label: "Dedupe window", Kind: "duration",
		Help: "Copies of the same packet within this time are one packet (e.g. 10m)."},
	{Key: "ingest.window_announce", Label: "Dedupe window, announces", Kind: "duration",
		Help: "Relays can queue announces for up to 3 h, so this is longer (e.g. 4h)."},
}

// EditableSettings lists the settings the admin page can change.
func (s *Store) EditableSettings() []SettingInfo {
	out := make([]SettingInfo, len(settingInfo))
	for i, si := range settingInfo {
		si.Value = s.Setting(si.Key)
		si.Default = defaultSettings[si.Key]
		out[i] = si
	}
	return out
}

var observerIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// ValidateSetting checks a value for an editable setting and returns it
// normalised.
func ValidateSetting(key, value string) (string, error) {
	value = strings.TrimSpace(value)
	for _, si := range settingInfo {
		if si.Key != key {
			continue
		}
		switch si.Kind {
		case "days":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 3650 {
				return "", fmt.Errorf("%s: a whole number of days from 1 to 3650", si.Label)
			}
			return strconv.Itoa(n), nil
		case "bool":
			switch strings.ToLower(value) {
			case "1", "true", "yes", "on":
				return "1", nil
			case "0", "false", "no", "off":
				return "0", nil
			}
			return "", fmt.Errorf("%s: on or off", si.Label)
		case "duration":
			d, err := time.ParseDuration(value)
			if err != nil || d < time.Minute || d > 24*time.Hour {
				return "", fmt.Errorf("%s: a duration from 1m to 24h, such as 10m or 4h", si.Label)
			}
			return d.String(), nil
		case "observer":
			if value != "" && !observerIDRe.MatchString(value) {
				return "", fmt.Errorf("%s: not a valid observer id", si.Label)
			}
			return value, nil
		}
	}
	return "", fmt.Errorf("unknown setting %q", key)
}

// Pin is a node position set by hand.
type Pin struct {
	Lat    float64   `json:"lat"`
	Lon    float64   `json:"lon"`
	Height *float64  `json:"height,omitempty"`
	Note   string    `json:"note,omitempty"`
	SetT   time.Time `json:"set_t"`
}

var ErrBadPosition = errors.New("store: latitude must be -90..90 and longitude -180..180, and not 0, 0 (which means no position)")

// ValidPos reports whether a position is usable. 0,0 is what devices send
// when no position is set, so it counts as none.
func ValidPos(lat, lon float64) bool {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 || lat != lat || lon != lon {
		return false
	}
	return !(lat == 0 && lon == 0)
}

func checkPin(p Pin) error {
	if !ValidPos(p.Lat, p.Lon) {
		return ErrBadPosition
	}
	if p.Height != nil && (*p.Height < -500 || *p.Height > 10000) {
		return errors.New("store: height must be -500..10000 m")
	}
	if len([]rune(p.Note)) > 200 {
		return errors.New("store: note is limited to 200 characters")
	}
	return nil
}

// SetObserverPin sets an observer's position by hand.
func (s *Store) SetObserverPin(id int64, p Pin) error {
	if err := checkPin(p); err != nil {
		return err
	}
	res, err := s.DB.Exec(`UPDATE observers SET pin_lat = ?, pin_lon = ?, pin_height = ?, pin_note = ?, pin_t = ?
		WHERE id = ?`, p.Lat, p.Lon, p.Height, nullStr(p.Note), ms(time.Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ForgetObserver deletes an observer and everything it heard: its
// receptions, channel stats, frame errors and hourly node stats. Packets
// no other observer heard go too, unless they are announces (announce
// history outlives receptions anyway). It returns the observer's ext id.
// An observer that is still sending comes back as new.
func (s *Store) ForgetObserver(id int64) (string, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var ext string
	if err := tx.QueryRow(`SELECT ext_id FROM observers WHERE id = ?`, id).Scan(&ext); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	for _, q := range []string{
		`CREATE TEMP TABLE IF NOT EXISTS forget_pk (id INTEGER PRIMARY KEY)`,
		`DELETE FROM forget_pk`,
		`INSERT OR IGNORE INTO forget_pk SELECT packet_id FROM receptions WHERE observer_id = ?1`,
		`DELETE FROM receptions WHERE observer_id = ?1`,
		`DELETE FROM channel_stats WHERE observer_id = ?1`,
		`DELETE FROM frame_errors WHERE observer_id = ?1`,
		`DELETE FROM node_stats_hourly WHERE observer_id = ?1`,
		`DELETE FROM packets WHERE id IN (SELECT id FROM forget_pk)
			AND NOT EXISTS (SELECT 1 FROM receptions r WHERE r.packet_id = packets.id)
			AND NOT EXISTS (SELECT 1 FROM announces a WHERE a.packet_id = packets.id)`,
		// rx_count counts whole receptions
		`UPDATE packets SET rx_count = (SELECT COUNT(*) FROM receptions r WHERE r.packet_id = packets.id AND r.partial = 0)
			WHERE id IN (SELECT id FROM forget_pk)`,
		`DELETE FROM forget_pk`,
		`DELETE FROM observers WHERE id = ?1`,
	} {
		var args []any
		if strings.Contains(q, "?1") {
			args = []any{id}
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(`DELETE FROM settings WHERE key = 'observer.local' AND value = ?`, ext); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if s.OnForget != nil {
		s.OnForget(ext)
	}
	return ext, nil
}

// ClearObserverPin removes an observer's hand-set position.
func (s *Store) ClearObserverPin(id int64) error {
	_, err := s.DB.Exec(`UPDATE observers SET pin_lat = NULL, pin_lon = NULL, pin_height = NULL, pin_note = NULL,
		pin_t = NULL WHERE id = ?`, id)
	return err
}

// SetPin pins a node (identity hash, hex) to a position.
func (s *Store) SetPin(identHex string, p Pin) error {
	ident, err := hex.DecodeString(identHex)
	if err != nil || len(ident) != 16 {
		return ErrNotFound
	}
	if err := checkPin(p); err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO node_pins (identity_hash, lat, lon, height, note, set_t) VALUES (?,?,?,?,?,?)
		ON CONFLICT(identity_hash) DO UPDATE SET lat = excluded.lat, lon = excluded.lon,
			height = excluded.height, note = excluded.note, set_t = excluded.set_t`,
		ident, p.Lat, p.Lon, p.Height, nullStr(p.Note), ms(time.Now()))
	return err
}

// NodeNameMax is the longest name that can be set by hand, in characters.
const NodeNameMax = 64

// manualName is the SQL for a node's name set by hand, given an expression
// for its identity hash; NULL when none is set.
func manualName(ident string) string {
	return `(SELECT name FROM node_names WHERE identity_hash = ` + ident + `)`
}

// CleanNodeName trims a name and checks it: up to NodeNameMax characters,
// no control characters. An empty result means no name.
func CleanNodeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > NodeNameMax {
		return "", fmt.Errorf("store: a name is limited to %d characters", NodeNameMax)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 {
			return "", errors.New("store: a name cannot contain control characters")
		}
	}
	return name, nil
}

// SetNodeName names a node (identity hash, hex) by hand; an empty name
// removes it, so the announced name shows again.
func (s *Store) SetNodeName(identHex, name string) error {
	ident, err := hex.DecodeString(identHex)
	if err != nil || len(ident) != 16 {
		return ErrNotFound
	}
	name, err = CleanNodeName(name)
	if err != nil {
		return err
	}
	if name == "" {
		_, err = s.DB.Exec(`DELETE FROM node_names WHERE identity_hash = ?`, ident)
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO node_names (identity_hash, name, set_t) VALUES (?,?,?)
		ON CONFLICT(identity_hash) DO UPDATE SET name = excluded.name, set_t = excluded.set_t`,
		ident, name, ms(time.Now()))
	return err
}

// DeletedNode says what DeleteNode removed.
type DeletedNode struct {
	Destinations int64 `json:"destinations"`
	Packets      int64 `json:"packets"` // only with its packets
}

// DeleteNode deletes a node (identity hash, hex): its destinations, its
// hourly signal stats, its name and position set by hand, and its link to
// the frames heard from it. Those frames stay in the feed, without its name,
// unless withPackets is set: then the packets it sent and all its announces
// (also the copies others relayed) go too, with their receptions. Packets
// others sent to it are kept. A node that is heard again comes back.
func (s *Store) DeleteNode(identHex string, withPackets bool) (DeletedNode, error) {
	var out DeletedNode
	ident, err := hex.DecodeString(identHex)
	if err != nil || len(ident) != 16 {
		return out, ErrNotFound
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var known bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM destinations WHERE identity_hash = ?1)
		OR EXISTS (SELECT 1 FROM receptions WHERE tx_identity = ?1)
		OR EXISTS (SELECT 1 FROM node_pins WHERE identity_hash = ?1)
		OR EXISTS (SELECT 1 FROM node_names WHERE identity_hash = ?1)`, ident).Scan(&known); err != nil {
		return out, err
	}
	if !known {
		return out, ErrNotFound
	}
	if withPackets {
		// Before the frames are unlinked from it below. Receptions and
		// announces go with their packets (ON DELETE CASCADE).
		res, err := tx.Exec(`DELETE FROM packets WHERE tx_identity = ?1
			OR id IN (SELECT packet_id FROM announces WHERE identity_hash = ?1)`, ident)
		if err != nil {
			return out, err
		}
		out.Packets, _ = res.RowsAffected()
	}
	res, err := tx.Exec(`DELETE FROM destinations WHERE identity_hash = ?`, ident)
	if err != nil {
		return out, err
	}
	out.Destinations, _ = res.RowsAffected()
	for _, q := range []string{
		`UPDATE receptions SET tx_identity = NULL WHERE tx_identity = ?`,
		`UPDATE packets SET tx_identity = NULL WHERE tx_identity = ?`,
		`DELETE FROM node_stats_hourly WHERE tx_identity = ?`,
		`DELETE FROM node_pins WHERE identity_hash = ?`,
		`DELETE FROM node_names WHERE identity_hash = ?`,
	} {
		if _, err := tx.Exec(q, ident); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

// ClearPin removes a node's pin.
func (s *Store) ClearPin(identHex string) error {
	ident, err := hex.DecodeString(identHex)
	if err != nil || len(ident) != 16 {
		return ErrNotFound
	}
	_, err = s.DB.Exec(`DELETE FROM node_pins WHERE identity_hash = ?`, ident)
	return err
}

// pinFor returns a node's pin, or nil.
func (s *Store) pinFor(ident []byte) (*Pin, error) {
	var p Pin
	var note sql.NullString
	var t int64
	err := s.DB.QueryRow(`SELECT lat, lon, height, note, set_t FROM node_pins WHERE identity_hash = ?`, ident).
		Scan(&p.Lat, &p.Lon, &p.Height, &note, &t)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Note, p.SetT = note.String, fromMs(t)
	return &p, nil
}

// StripPayloads removes stored bytes of non-announce packets (announces are
// kept whole) and returns how many packets were stripped.
func (s *Store) StripPayloads() (int64, error) {
	res, err := s.DB.Exec(`UPDATE packets SET payload = NULL WHERE payload IS NOT NULL AND pkt_type <> 1`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
