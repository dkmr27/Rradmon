package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/reassembly"
	"github.com/dkmr27/Rradmon/internal/store"
)

var radio = Radio{FreqHz: 925875000, BandwidthHz: 250000, SF: 9, CR: 5}
var t0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// splitPacket returns a 300-byte DATA packet as two RNode frames.
func splitPacket(seq byte) (f1, f2 []byte) {
	pkt := make([]byte, 300)
	pkt[0], pkt[1] = 0x00, 0 // DATA, single, hops 0
	for i := 2; i < len(pkt); i++ {
		pkt[i] = byte(i * 7)
	}
	hdr := seq<<4 | 0x01
	f1 = append([]byte{hdr}, pkt[:254]...)
	f2 = append([]byte{hdr}, pkt[254:]...)
	return
}

func setup(t *testing.T) (*Ingestor, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	in, err := New(st, radio, nil)
	if err != nil {
		t.Fatal(err)
	}
	return in, st
}

func frame(ms int, data []byte, rssi float32) reassembly.Frame {
	return reassembly.Frame{T: t0.Add(time.Duration(ms) * time.Millisecond), RSSI: rssi, SNR: 5, HasSignal: true, Data: data}
}

func count(t *testing.T, st *store.Store, q string) int {
	var n int
	if err := st.DB.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Observer A hears both halves; observer B hears only the first half.
func TestPartialAfterWhole(t *testing.T) {
	in, st := setup(t)
	f1, f2 := splitPacket(3)
	must(t, in.Frame("A", "test", frame(0, f1, -90), true))
	must(t, in.Frame("A", "test", frame(700, f2, -91), true))
	must(t, in.Frame("B", "test", frame(5, f1, -110), true))
	must(t, in.Flush(t0.Add(10*time.Second)))

	if n := count(t, st, `SELECT COUNT(*) FROM packets`); n != 1 {
		t.Fatalf("packets: %d", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM receptions WHERE partial = 1 AND hops = 0`); n != 1 {
		t.Errorf("partial first-half receptions: %d, want 1", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM frame_errors WHERE matched_packet_id IS NOT NULL`); n != 1 {
		t.Errorf("matched orphans: %d", n)
	}
}

// Observer B's lone second half arrives before A delivers the whole packet.
func TestPartialBeforeWhole(t *testing.T) {
	in, st := setup(t)
	f1, f2 := splitPacket(5)
	must(t, in.Frame("B", "test", frame(690, f2, -112), true))
	must(t, in.Frame("A", "test", frame(0, f1, -90), true))
	must(t, in.Frame("A", "test", frame(700, f2, -91), true))

	if n := count(t, st, `SELECT COUNT(*) FROM receptions WHERE partial = 2 AND hops IS NULL`); n != 1 {
		t.Errorf("partial second-half receptions: %d, want 1", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM receptions`); n != 2 {
		t.Errorf("receptions: %d, want 2", n)
	}
}

// An orphan that matches nothing stays a frame error.
func TestUnmatchedOrphan(t *testing.T) {
	in, st := setup(t)
	f1, _ := splitPacket(7)
	must(t, in.Frame("A", "test", frame(0, f1, -90), true))
	must(t, in.Flush(t0.Add(10*time.Second)))
	if n := count(t, st, `SELECT COUNT(*) FROM frame_errors WHERE kind = 'first half' AND matched_packet_id IS NULL`); n != 1 {
		t.Errorf("unmatched first half: %d", n)
	}
}

func TestDedupeWindow(t *testing.T) {
	in, st := setup(t)
	pkt := append([]byte{0x30, 0x00, 0}, make([]byte, 40)...) // header byte, DATA, hops 0
	must(t, in.Frame("A", "test", frame(0, pkt, -90), true))
	must(t, in.Frame("A", "test", frame(9*60*1000, pkt, -90), true))  // inside 10 min
	must(t, in.Frame("A", "test", frame(25*60*1000, pkt, -90), true)) // outside: repeat
	if n := count(t, st, `SELECT COUNT(*) FROM packets`); n != 2 {
		t.Errorf("packets: %d, want 2", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM packets WHERE repeat_of IS NOT NULL`); n != 1 {
		t.Errorf("repeats: %d, want 1", n)
	}
}

func TestPurge(t *testing.T) {
	in, st := setup(t)
	pkt := append([]byte{0x30, 0x00, 0}, make([]byte, 40)...)
	must(t, in.Frame("A", "test", frame(0, pkt, -90), true))
	r, err := st.Purge(context.Background(), t0.Add(15*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r.Receptions != 1 || r.Packets != 1 {
		t.Errorf("purge: %+v", r)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
