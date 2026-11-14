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

var ErrBadPosition = errors.New("store: latitude must be -90..90 and longitude -180..180")

// SetPin pins a node (identity hash, hex) to a position.
func (s *Store) SetPin(identHex string, p Pin) error {
	ident, err := hex.DecodeString(identHex)
	if err != nil || len(ident) != 16 {
		return ErrNotFound
	}
	if p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180 {
		return ErrBadPosition
	}
	if p.Height != nil && (*p.Height < -500 || *p.Height > 10000) {
		return errors.New("store: height must be -500..10000 m")
	}
	if len([]rune(p.Note)) > 200 {
		return errors.New("store: note is limited to 200 characters")
	}
	_, err = s.DB.Exec(`INSERT INTO node_pins (identity_hash, lat, lon, height, note, set_t) VALUES (?,?,?,?,?,?)
		ON CONFLICT(identity_hash) DO UPDATE SET lat = excluded.lat, lon = excluded.lon,
			height = excluded.height, note = excluded.note, set_t = excluded.set_t`,
		ident, p.Lat, p.Lon, p.Height, nullStr(p.Note), ms(time.Now()))
	return err
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
