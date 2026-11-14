package ingest

import (
	"context"
	"testing"
	"time"
)

// pkt returns a small DATA packet as one RNode frame; n makes it distinct.
func pkt(n byte) []byte {
	b := append([]byte{0x30, 0x00, 0}, make([]byte, 40)...)
	b[10] = n
	return b
}

func TestForgetObserver(t *testing.T) {
	in, st := setup(t)
	st.OnForget = in.Forget
	must(t, in.Frame("A", "test", frame(0, pkt(1), -90), true))
	must(t, in.Frame("A", "test", frame(100, pkt(2), -90), true))
	must(t, in.Frame("B", "test", frame(5, pkt(1), -100), true))
	must(t, in.Frame("B", "test", frame(200, pkt(3), -100), true)) // only B hears this one
	f1, _ := splitPacket(5)
	must(t, in.Frame("B", "test", frame(300, f1, -100), true)) // an orphan half

	var idB int64
	must(t, st.DB.QueryRow(`SELECT id FROM observers WHERE ext_id = 'B'`).Scan(&idB))
	ext, err := st.ForgetObserver(idB)
	if err != nil || ext != "B" {
		t.Fatalf("forget: %q %v", ext, err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM observers`); n != 1 {
		t.Errorf("observers: %d, want 1", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM receptions`); n != 2 {
		t.Errorf("receptions: %d, want A's 2", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM packets`); n != 2 {
		t.Errorf("packets: %d, want 2 (the one only B heard goes)", n)
	}
	if n := count(t, st, `SELECT SUM(rx_count) FROM packets`); n != 2 {
		t.Errorf("rx_count total: %d, want 2", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM frame_errors`); n != 0 {
		t.Errorf("frame errors: %d, want 0", n)
	}
	if _, err := st.ForgetObserver(idB); err == nil {
		t.Error("forgetting twice: no error")
	}

	// The local observer setting goes with it, and an observer still
	// sending comes back as new instead of failing on its old id
	must(t, st.SetSetting("observer.local", "A"))
	var idA int64
	must(t, st.DB.QueryRow(`SELECT id FROM observers WHERE ext_id = 'A'`).Scan(&idA))
	if _, err := st.ForgetObserver(idA); err != nil {
		t.Fatal(err)
	}
	if v := st.Setting("observer.local"); v != "" {
		t.Errorf("observer.local still %q", v)
	}
	must(t, in.Frame("A", "test", frame(1000, pkt(4), -90), true))
	if n := count(t, st, `SELECT COUNT(*) FROM observers WHERE ext_id = 'A'`); n != 1 {
		t.Errorf("A did not come back")
	}
}

func TestPurgeExpiresSilentObservers(t *testing.T) {
	in, st := setup(t)
	must(t, in.Frame("A", "test", frame(0, pkt(1), -90), true))
	day := 24 * time.Hour
	// After 15 days the receptions are gone but the observer stays (30 days)
	r, err := st.Purge(context.Background(), t0.Add(15*day))
	if err != nil || r.Observers != 0 {
		t.Fatalf("15 days: %+v %v", r, err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM observers`); n != 1 {
		t.Fatalf("observers after 15 days: %d", n)
	}
	r, err = st.Purge(context.Background(), t0.Add(31*day))
	if err != nil || r.Observers != 1 {
		t.Fatalf("31 days: %+v %v", r, err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM observers`); n != 0 {
		t.Errorf("observers after 31 days: %d", n)
	}
	// Receptions kept longer than observers: an observer with receptions left stays
	must(t, st.SetSetting("retain.receptions_days", "90"))
	must(t, in.Frame("B", "test", frame(0, pkt(2), -90), true))
	if r, err = st.Purge(context.Background(), t0.Add(40*day)); err != nil || r.Observers != 0 {
		t.Errorf("observer with receptions expired: %+v %v", r, err)
	}
}
