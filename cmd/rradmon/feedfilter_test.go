package main

import (
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/ingest"
	"github.com/dkmr27/Rradmon/internal/reassembly"
	"github.com/dkmr27/Rradmon/internal/store"
)

// TestFeedFilters narrows the live feed by destination, transmitter and
// observer, by hash and by name, on the bench capture.
func TestFeedFilters(t *testing.T) {
	st := openTestStore(t)
	if _, err := importCapture(st, "../../testdata/rig-corpus1.jsonl", true); err != nil {
		t.Fatal(err)
	}
	const rrptr = "135a9914ef8e82c36ea3c82bd4689e02" // Testing-Rrptr's identity
	now := time.Now().Add(time.Hour)
	feed := func(f store.FeedFilter) []store.PacketRow {
		t.Helper()
		rows, err := st.PacketFeed(now, time.Time{}, f, 5000)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	keys := func(rows []store.PacketRow) map[int64]bool {
		m := map[int64]bool{}
		for _, r := range rows {
			if r.Kind == "packet" {
				m[r.ID] = true
			}
		}
		return m
	}
	same := func(name string, a, b []store.PacketRow) {
		t.Helper()
		ka, kb := keys(a), keys(b)
		if len(ka) != len(kb) {
			t.Errorf("%s: %d rows, want %d", name, len(ka), len(kb))
			return
		}
		for k := range ka {
			if !kb[k] {
				t.Errorf("%s: packet %d not expected", name, k)
			}
		}
	}

	all := feed(store.FeedFilter{})
	if len(all) == 0 {
		t.Fatal("empty feed")
	}

	// Destination: the busiest one in the capture
	count := map[string]int{}
	names := map[string]string{}
	for _, r := range all {
		if r.Kind == "packet" {
			count[r.Dest]++
			if r.DestName != "" {
				names[r.Dest] = r.DestName
			}
		}
	}
	var dest string
	for d, n := range count {
		if names[d] != "" && (dest == "" || n > count[dest]) {
			dest = d
		}
	}
	if dest == "" {
		t.Fatal("no named destination in the capture")
	}
	byDest := feed(store.FeedFilter{Dest: dest})
	for _, r := range byDest {
		if r.Kind != "packet" || r.Dest != dest {
			t.Fatalf("dest filter returned %s to %s", r.Kind, r.Dest)
		}
	}
	if len(byDest) != count[dest] {
		t.Errorf("dest filter: %d rows, want %d", len(byDest), count[dest])
	}
	same("dest by prefix", feed(store.FeedFilter{Dest: dest[:8]}), byDest)
	byName := feed(store.FeedFilter{Dest: names[dest]})
	for k := range keys(byDest) {
		if !keys(byName)[k] {
			t.Errorf("dest by name %q misses packet %d", names[dest], k)
		}
	}

	// Transmitter: by identity, its start, and its announced name
	byTx := feed(store.FeedFilter{From: rrptr})
	if len(byTx) == 0 {
		t.Fatal("no packets from Rrptr")
	}
	for _, r := range all {
		if r.Tx == rrptr && !keys(byTx)[r.ID] {
			t.Errorf("from filter misses packet %d", r.ID)
		}
	}
	same("from by prefix", feed(store.FeedFilter{From: rrptr[:6]}), byTx)
	same("from by name", feed(store.FeedFilter{From: "testing-RRPTR"}), byTx)

	// Observer: every row first heard by it is there, and nothing for an
	// unknown one; orphans stay in, since an observer filter can place them
	obs := all[0].Observer
	byObs := feed(store.FeedFilter{Observer: obs})
	for _, r := range all {
		if r.Kind == "packet" && r.Observer == obs && !keys(byObs)[r.ID] {
			t.Errorf("observer filter misses packet %d", r.ID)
		}
	}
	if len(byObs) > len(all) {
		t.Errorf("observer filter: %d rows of %d", len(byObs), len(all))
	}
	if n := len(feed(store.FeedFilter{Observer: "no-such-observer"})); n != 0 {
		t.Errorf("unknown observer: %d rows", n)
	}

	// Filters combine; LIKE wildcards in a name are taken literally
	for _, r := range feed(store.FeedFilter{Dest: dest, Observer: obs}) {
		if r.Dest != dest || !keys(byObs)[r.ID] {
			t.Errorf("dest and observer: packet %d does not match both", r.ID)
		}
	}
	for _, q := range []string{"%", "_"} {
		if n := len(feed(store.FeedFilter{Dest: q})); n != 0 {
			t.Errorf("dest %q: %d rows, want 0", q, n)
		}
	}
}

// An observer filter finds packets that observer heard after another one
// did, not only those it heard first.
func TestFeedFilterSecondObserver(t *testing.T) {
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
	for _, step := range []struct {
		obs string
		fr  reassembly.Frame
	}{{"A", f(0, pkt(5))}, {"B", f(10, pkt(5))}, {"A", f(500, pkt(9))}} {
		if err := in.Frame(step.obs, "mqtt", step.fr, true); err != nil {
			t.Fatal(err)
		}
	}
	for obs, want := range map[string]int{"A": 2, "B": 1} {
		rows, err := st.PacketFeed(time.Now().Add(time.Hour), time.Time{}, store.FeedFilter{Observer: obs}, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != want {
			t.Errorf("observer %s: %d rows, want %d", obs, len(rows), want)
		}
		// Catching up keeps the filter
		rows, _ = st.PacketFeed(time.Time{}, t0.Add(-time.Second), store.FeedFilter{Observer: obs}, 50)
		if len(rows) != want {
			t.Errorf("observer %s catching up: %d rows, want %d", obs, len(rows), want)
		}
	}
}
