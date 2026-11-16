// Package mqtt carries observer data over MQTT: the v1 message schema
// (docs/spec-v0.md §5), a publisher for observers and a subscriber for the
// server. Everything received is untrusted and validated.
package mqtt

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const Version = 1

// Radio is the LoRa setting an observer listens on.
type Radio struct {
	FreqHz uint32 `json:"freq_hz"`
	BwHz   uint32 `json:"bw_hz"`
	SF     uint8  `json:"sf"`
	CR     uint8  `json:"cr"`
	Sync   uint8  `json:"sync,omitempty"`
}

// NetworkID is the topic segment for a radio setting, e.g.
// "925875000-250000-9-5". It matches ingest.Radio.Slug.
func (r Radio) NetworkID() string {
	return fmt.Sprintf("%d-%d-%d-%d", r.FreqHz, r.BwHz, r.SF, r.CR)
}

// Rx is one received LoRa frame (topic …/rx).
type Rx struct {
	V        int    `json:"v"`
	Observer string `json:"observer"`
	Seq      uint64 `json:"seq"`
	TObs     string `json:"t_obs"` // RFC 3339, the observer's clock
	// Radio was sent by observers up to 0.2.0; the topic's network and the
	// status say the same. When present it must match the topic.
	Radio *Radio   `json:"radio,omitempty"`
	RSSI  *float32 `json:"rssi,omitempty"`
	SNR   *float32 `json:"snr,omitempty"`
	Len   int      `json:"len"`
	// Frame is the raw LoRa frame including the RNode header byte (Raw
	// true), or a whole Reticulum packet from an RNode in normal mode.
	Frame       []byte `json:"frame"` // base64 in JSON
	Raw         *bool  `json:"raw,omitempty"`
	Stripped    bool   `json:"stripped,omitempty"`
	FrameHash   string `json:"frame_hash,omitempty"`
	ObserverKey string `json:"observer_key,omitempty"` // reserved
	Sig         string `json:"sig,omitempty"`          // reserved
}

// IsRaw reports whether Frame includes the RNode header byte (the default).
func (m *Rx) IsRaw() bool { return m.Raw == nil || *m.Raw }

// Chan is an observer's channel report (topic …/chan).
type Chan struct {
	V            int      `json:"v"`
	Observer     string   `json:"observer"`
	TObs         string   `json:"t_obs"`
	AirtimeShort float32  `json:"airtime_s"`
	AirtimeLong  float32  `json:"airtime_l"`
	UtilShort    float32  `json:"util_s"`
	UtilLong     float32  `json:"util_l"`
	RSSI         float32  `json:"rssi"`
	NoiseFloor   float32  `json:"noise_floor"`
	Interference *float32 `json:"interference,omitempty"`
}

// Status is retained on …/status, and is also the observer's last will.
type Status struct {
	V        int      `json:"v"`
	Observer string   `json:"observer"`
	Online   bool     `json:"online"`
	Firmware string   `json:"fw,omitempty"`
	Software string   `json:"sw,omitempty"`
	Radio    *Radio   `json:"radio,omitempty"`
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	UptimeS  int64    `json:"uptime_s,omitempty"`
	TObs     string   `json:"t_obs,omitempty"`
}

// Topic layout: <prefix>/<network>/<observer>/<kind>.
const DefaultPrefix = "rradmon/v1"

func Topic(prefix, network, observer, kind string) string {
	return strings.Join([]string{prefix, network, observer, kind}, "/")
}

// ParseTopic splits a topic under prefix into network, observer and kind.
func ParseTopic(prefix, topic string) (network, observer, kind string, ok bool) {
	rest, found := strings.CutPrefix(topic, prefix+"/")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

var observerRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidObserverID reports whether id is safe to use in topics and the UI.
func ValidObserverID(id string) bool { return observerRe.MatchString(id) }

var (
	errVersion = errors.New("unsupported version")
	errObs     = errors.New("observer id does not match topic or is invalid")
)

// Validate checks an Rx against its topic's observer and returns the observer
// timestamp (zero if absent or implausible).
func (m *Rx) Validate(topicObserver string, now time.Time) (time.Time, error) {
	if m.V != Version {
		return time.Time{}, errVersion
	}
	if m.Observer != topicObserver || !ValidObserverID(m.Observer) {
		return time.Time{}, errObs
	}
	maxLen := 255 // RNode header + 254
	if !m.IsRaw() {
		maxLen = 508
	}
	if len(m.Frame) < 2 || len(m.Frame) > maxLen {
		return time.Time{}, fmt.Errorf("frame length %d out of range", len(m.Frame))
	}
	if m.Stripped {
		return time.Time{}, errors.New("stripped frames are not accepted from observers")
	}
	if m.RSSI != nil && (*m.RSSI < -160 || *m.RSSI > 10) {
		return time.Time{}, fmt.Errorf("rssi %v out of range", *m.RSSI)
	}
	if m.SNR != nil && (*m.SNR < -32 || *m.SNR > 32) {
		return time.Time{}, fmt.Errorf("snr %v out of range", *m.SNR)
	}
	if (m.RSSI == nil) != (m.SNR == nil) {
		return time.Time{}, errors.New("rssi and snr must both be present or absent")
	}
	return plausibleTime(m.TObs, now), nil
}

// Validate checks a Chan report.
func (m *Chan) Validate(topicObserver string, now time.Time) (time.Time, error) {
	if m.V != Version {
		return time.Time{}, errVersion
	}
	if m.Observer != topicObserver || !ValidObserverID(m.Observer) {
		return time.Time{}, errObs
	}
	for _, p := range []float32{m.AirtimeShort, m.AirtimeLong, m.UtilShort, m.UtilLong} {
		if p < 0 || p > 100 {
			return time.Time{}, fmt.Errorf("percentage %v out of range", p)
		}
	}
	if m.NoiseFloor < -160 || m.NoiseFloor > 10 || m.RSSI < -160 || m.RSSI > 10 {
		return time.Time{}, errors.New("rssi/noise floor out of range")
	}
	return plausibleTime(m.TObs, now), nil
}

// plausibleTime parses an observer timestamp, ignoring one more than a day
// away from the server's clock.
func plausibleTime(s string, now time.Time) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	if d := t.Sub(now); d > 24*time.Hour || d < -24*time.Hour {
		return time.Time{}
	}
	return t
}
