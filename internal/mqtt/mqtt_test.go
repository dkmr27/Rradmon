package mqtt

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var radio = Radio{FreqHz: 925875000, BwHz: 250000, SF: 9, CR: 5}
var now = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

type recorder struct {
	rx     []*Rx
	chans  int
	status int
}

func (r *recorder) Rx(_, _ string, m *Rx, _, _ time.Time)                { r.rx = append(r.rx, m) }
func (r *recorder) Chan(_, _ string, _ *Chan, _, _ time.Time)            { r.chans++ }
func (r *recorder) Status(_, _ string, _ *Status, _ []byte, _ time.Time) { r.status++ }

func newSub(h Handler, allow ...string) *Subscriber {
	s := &Subscriber{prefix: DefaultPrefix, h: h, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		buckets: map[string]*bucket{}, lastSeq: map[string]uint64{}, stats: map[string]*SubStats{}}
	if len(allow) > 0 {
		s.allow = map[string]bool{}
		for _, a := range allow {
			s.allow[a] = true
		}
	}
	return s
}

func rxMsg(obs string, seq uint64, frame []byte) []byte {
	rssi, snr := float32(-90), float32(8.5)
	b, _ := json.Marshal(Rx{V: 1, Observer: obs, Seq: seq, TObs: now.Format(time.RFC3339Nano),
		Radio: radio, RSSI: &rssi, SNR: &snr, Len: len(frame), Frame: frame})
	return b
}

func topic(obs, kind string) string { return Topic(DefaultPrefix, radio.NetworkID(), obs, kind) }

func TestTopicRoundTrip(t *testing.T) {
	tp := topic("obs-a", "rx")
	if tp != "rradmon/v1/925875000-250000-9-5/obs-a/rx" {
		t.Fatalf("topic %q", tp)
	}
	n, o, k, ok := ParseTopic(DefaultPrefix, tp)
	if !ok || n != "925875000-250000-9-5" || o != "obs-a" || k != "rx" {
		t.Fatalf("parse: %q %q %q %v", n, o, k, ok)
	}
	if _, _, _, ok := ParseTopic(DefaultPrefix, "rradmon/v1/x/y/z/extra"); ok {
		t.Error("accepted extra level")
	}
}

func TestAcceptsValidRx(t *testing.T) {
	rec := &recorder{}
	s := newSub(rec)
	s.handle(topic("obs-a", "rx"), rxMsg("obs-a", 1, []byte{0x30, 0x00, 0x00, 1, 2, 3}), now)
	if len(rec.rx) != 1 || *rec.rx[0].RSSI != -90 || rec.rx[0].Frame[0] != 0x30 {
		t.Fatalf("got %+v", rec.rx)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]struct {
		topic, payload string
	}{
		"observer mismatch": {topic("obs-b", "rx"), string(rxMsg("obs-a", 1, []byte{1, 2, 3}))},
		"bad observer id":   {topic("obs a", "rx"), string(rxMsg("obs a", 1, []byte{1, 2, 3}))},
		"frame too long":    {topic("obs-a", "rx"), string(rxMsg("obs-a", 1, make([]byte, 300)))},
		"frame too short":   {topic("obs-a", "rx"), string(rxMsg("obs-a", 1, []byte{1}))},
		"not json":          {topic("obs-a", "rx"), "hello"},
		"wrong version":     {topic("obs-a", "rx"), strings.Replace(string(rxMsg("obs-a", 1, []byte{1, 2, 3})), `"v":1`, `"v":2`, 1)},
		"rssi out of range": {topic("obs-a", "rx"), strings.Replace(string(rxMsg("obs-a", 1, []byte{1, 2, 3})), `"rssi":-90`, `"rssi":-900`, 1)},
		"network mismatch":  {"rradmon/v1/868000000-125000-8-5/obs-a/rx", string(rxMsg("obs-a", 1, []byte{1, 2, 3}))},
		"oversize":          {topic("obs-a", "rx"), strings.Repeat("x", maxMessageBytes+1)},
	}
	for name, c := range cases {
		rec := &recorder{}
		s := newSub(rec)
		s.handle(c.topic, []byte(c.payload), now)
		if len(rec.rx) != 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAllowList(t *testing.T) {
	rec := &recorder{}
	s := newSub(rec, "obs-a")
	s.handle(topic("obs-b", "rx"), rxMsg("obs-b", 1, []byte{1, 2, 3}), now)
	s.handle(topic("obs-a", "rx"), rxMsg("obs-a", 1, []byte{1, 2, 3}), now)
	if len(rec.rx) != 1 || rec.rx[0].Observer != "obs-a" {
		t.Fatalf("got %d", len(rec.rx))
	}
}

func TestRateLimit(t *testing.T) {
	rec := &recorder{}
	s := newSub(rec)
	for i := 1; i <= rateBurst+50; i++ {
		s.handle(topic("obs-a", "rx"), rxMsg("obs-a", uint64(i), []byte{1, 2, 3}), now)
	}
	if len(rec.rx) != rateBurst || s.Stats()["obs-a"].RateLimited != 50 {
		t.Fatalf("accepted %d, limited %d", len(rec.rx), s.Stats()["obs-a"].RateLimited)
	}
	// Tokens refill over time.
	s.handle(topic("obs-a", "rx"), rxMsg("obs-a", 999, []byte{1, 2, 3}), now.Add(time.Second))
	if len(rec.rx) != rateBurst+1 {
		t.Error("bucket did not refill")
	}
}

func TestSeqGaps(t *testing.T) {
	rec := &recorder{}
	s := newSub(rec)
	for _, seq := range []uint64{1, 2, 5, 6, 1, 2} { // gap of 2, then observer restart
		s.handle(topic("obs-a", "rx"), rxMsg("obs-a", seq, []byte{1, 2, 3}), now)
	}
	if g := s.Stats()["obs-a"].SeqGaps; g != 2 {
		t.Errorf("seq gaps %d, want 2", g)
	}
}

func TestImplausibleObserverTime(t *testing.T) {
	m := Rx{V: 1, Observer: "o", Radio: radio, Frame: []byte{1, 2}, TObs: now.Add(-48 * time.Hour).Format(time.RFC3339)}
	tObs, err := m.Validate("o", now)
	if err != nil || !tObs.IsZero() {
		t.Errorf("tObs %v err %v", tObs, err)
	}
}

func TestNoCredentialsInURL(t *testing.T) {
	_, err := BrokerConfig{URL: "tcp://user:pass@host:1883"}.options(slog.Default())
	if err == nil {
		t.Error("accepted credentials in the URL")
	}
}
