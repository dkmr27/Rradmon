package main

import (
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/ingest"
	"github.com/dkmr27/Rradmon/internal/reassembly"
	"github.com/dkmr27/Rradmon/internal/store"
)

func findObs(t *testing.T, vs []store.ObserverView, id string) store.ObserverView {
	t.Helper()
	for _, v := range vs {
		if v.ExtID == id {
			return v
		}
	}
	t.Fatalf("observer %s not listed", id)
	return store.ObserverView{}
}

// The corpus is one observer; its figures come from its receptions.
func TestObserversFromCorpus(t *testing.T) {
	st := openTestStore(t)
	if _, err := importCapture(st, "../../testdata/rig-corpus1.jsonl", true); err != nil {
		t.Fatal(err)
	}
	// "now" just after the capture ended
	var last int64
	st.DB.QueryRow(`SELECT MAX(t_rx) FROM receptions`).Scan(&last)
	now := time.UnixMilli(last).Add(time.Minute)

	vs, err := st.Observers(now)
	if err != nil {
		t.Fatal(err)
	}
	o := findObs(t, vs, "rnode:COM32")
	if o.Rx24h != 30 || o.Packets24h != 27 || o.OnlyHere24h != 27 {
		t.Errorf("rx %d packets %d only-here %d, want 30/27/27", o.Rx24h, o.Packets24h, o.OnlyHere24h)
	}
	if o.State != "online" || o.NoiseFloor == nil || o.AvgRSSI1h == nil {
		t.Errorf("state %q noise %v rssi %v", o.State, o.NoiseFloor, o.AvgRSSI1h)
	}
	// Ten minutes of silence later it is shown as silent.
	vs, _ = st.Observers(now.Add(10 * time.Minute))
	if o := findObs(t, vs, "rnode:COM32"); o.State != "silent" {
		t.Errorf("after silence: state %q", o.State)
	}
}

// Two observers hear one packet; a third packet is heard only by B.
func TestObserversOnlyHere(t *testing.T) {
	st := openTestStore(t)
	in, err := ingest.New(st, ingest.Radio{FreqHz: 925875000, BandwidthHz: 250000, SF: 9, CR: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Minute)
	pkt := func(b byte) []byte { return append([]byte{0x30, 0x00, 0x00}, make([]byte, 40)...)[:3+16+1+int(b)] }
	f := func(ms int, d []byte) reassembly.Frame {
		return reassembly.Frame{T: t0.Add(time.Duration(ms) * time.Millisecond), RSSI: -90, SNR: 5, HasSignal: true, Data: d}
	}
	shared, onlyB := pkt(5), pkt(9)
	for _, step := range []struct {
		obs string
		fr  reassembly.Frame
	}{{"A", f(0, shared)}, {"B", f(10, shared)}, {"B", f(500, onlyB)}} {
		if err := in.Frame(step.obs, "mqtt", step.fr, true); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := st.Observers(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a := findObs(t, vs, "A"); a.Packets24h != 1 || a.OnlyHere24h != 0 {
		t.Errorf("A: packets %d only-here %d", a.Packets24h, a.OnlyHere24h)
	}
	if b := findObs(t, vs, "B"); b.Packets24h != 2 || b.OnlyHere24h != 1 {
		t.Errorf("B: packets %d only-here %d", b.Packets24h, b.OnlyHere24h)
	}
}

// An offline status (an MQTT last will, perhaps retained from long ago)
// marks the observer offline without making it look recently heard.
func TestObserverOfflineStatus(t *testing.T) {
	st := openTestStore(t)
	long := time.Now().Add(-48 * time.Hour)
	if _, err := st.Observer("obs-x", "mqtt", long); err != nil {
		t.Fatal(err)
	}
	if err := st.ObserverStatus("obs-x", "mqtt", time.Now(), store.ObserverState{Online: false}); err != nil {
		t.Fatal(err)
	}
	vs, _ := st.Observers(time.Now())
	o := findObs(t, vs, "obs-x")
	if o.State != "offline" || time.Since(o.LastSeen) < 47*time.Hour {
		t.Errorf("state %q last seen %v", o.State, o.LastSeen)
	}
	raw := `{"v":1,"observer":"obs-x","online":true,"fw":"Heltec V3 SX1262","uptime_s":120,"battery_mv":4100,"secret":"x"}`
	if err := st.ObserverStatus("obs-x", "mqtt", time.Now(), store.ObserverState{Online: true,
		Firmware: "Heltec V3 SX1262", Raw: raw}); err != nil {
		t.Fatal(err)
	}
	vs, _ = st.Observers(time.Now())
	o = findObs(t, vs, "obs-x")
	if o.State != "online" || o.Firmware != "Heltec V3 SX1262" || o.Status["uptime_s"] != 120.0 || o.Status["battery_mv"] != 4100.0 {
		t.Errorf("after online status: %+v", o)
	}
	if _, leaked := o.Status["secret"]; leaked {
		t.Error("unknown status field passed through")
	}
}
