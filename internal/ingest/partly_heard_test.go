package ingest

import (
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/reticulum"
	"github.com/dkmr27/Rradmon/internal/store"
)

// Today's bench case (2026-11-13 20:18:10): a split probe reached both
// observers only as its first half, yet the receiver proved it. The two
// orphans should form one group, and the proof should be flagged as proving
// an unheard packet and linked to that group.

// probeFrames returns a 300-byte probe split over two frames, and a frame
// with the proof the receiver sends back for it.
func probeFrames(t *testing.T) (f1, f2, proof []byte) {
	t.Helper()
	pkt := make([]byte, 300)
	pkt[0], pkt[1] = 0x00, 0 // DATA, single, header 1, hops 0
	for i := 0; i < 16; i++ {
		pkt[2+i] = 0x14 // destination
	}
	for i := 19; i < len(pkt); i++ {
		pkt[i] = byte(i * 13)
	}
	p, err := reticulum.Parse(pkt)
	if err != nil {
		t.Fatal(err)
	}
	f1 = append([]byte{0x71}, pkt[:254]...)
	f2 = append([]byte{0x71}, pkt[254:]...)
	pr := []byte{0x40, byte(reticulum.TypeProof), 0} // RNode header, PROOF single, hops 0
	pr = append(pr, p.TruncatedHash()...)            // addressed to the proved packet
	pr = append(pr, 0)                               // context
	pr = append(pr, make([]byte, 64)...)             // signature
	return f1, f2, pr
}

func TestPartlyHeardBothObservers(t *testing.T) {
	in, st := setup(t)
	f1, _, proof := probeFrames(t)
	must(t, in.Frame("A", "test", frame(0, f1, -94), true))
	must(t, in.Frame("B", "test", frame(5, f1, -98), true))
	// The proof displaces each observer's waiting first half, making it an orphan
	must(t, in.Frame("A", "test", frame(1300, proof, -60), true))
	must(t, in.Frame("B", "test", frame(1310, proof, -72), true))
	checkPartlyHeard(t, st, 2)
}

// B's messages arrive late (MQTT lag): A's proof is stored before B's orphan.
func TestPartlyHeardLateObserver(t *testing.T) {
	in, st := setup(t)
	f1, _, proof := probeFrames(t)
	must(t, in.Frame("A", "test", frame(0, f1, -94), true))
	must(t, in.Frame("A", "test", frame(1300, proof, -60), true))
	must(t, in.Frame("B", "test", frame(5, f1, -98), true))
	must(t, in.Flush(t0.Add(10*time.Second))) // B's half times out
	checkPartlyHeard(t, st, 1)
}

func checkPartlyHeard(t *testing.T, st *store.Store, wantProofRows int) {
	t.Helper()
	if n := count(t, st, `SELECT COUNT(*) FROM frame_errors WHERE kind = 'first half' AND matched_packet_id IS NULL`); n != 2 {
		t.Fatalf("orphans: %d, want 2", n)
	}
	if n := count(t, st, `SELECT COUNT(DISTINCT group_id) FROM frame_errors`); n != 1 {
		t.Errorf("orphan groups: %d, want 1", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM frame_errors WHERE proof_packet_id IS NOT NULL`); n != 2 {
		t.Errorf("orphans linked to the proof: %d, want 2", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM packets WHERE pkt_type = 3 AND proves_unheard = 1 AND proves_orphan IS NOT NULL`); n != 1 {
		t.Errorf("flagged and linked proofs: %d, want 1", n)
	}

	rows, err := st.Feed(t0.Add(time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	var orphans, proofs int
	for _, r := range rows {
		switch {
		case r.Kind == "orphan":
			orphans++
			if len(r.GroupObservers) != 2 || r.LinkID == 0 || r.LinkDelayMs <= 0 {
				t.Errorf("orphan row: observers %v link %d delay %d", r.GroupObservers, r.LinkID, r.LinkDelayMs)
			}
			if r.Type != "DATA" || r.Dest == "" || r.Hops == nil {
				t.Errorf("orphan row header not parsed: %+v", r)
			}
		case r.Type == "PROOF":
			proofs++
			if !r.ProvesUnheard || r.LinkID == 0 {
				t.Errorf("proof row: unheard %v link %d", r.ProvesUnheard, r.LinkID)
			}
		}
	}
	if orphans != 1 || proofs != wantProofRows {
		t.Errorf("feed: %d orphan rows (want 1, one per group), %d proof rows (want %d)", orphans, proofs, wantProofRows)
	}
}

// A proof of a packet that was heard whole is not flagged.
func TestProofOfHeardPacket(t *testing.T) {
	in, st := setup(t)
	f1, f2, proof := probeFrames(t)
	must(t, in.Frame("A", "test", frame(0, f1, -94), true))
	must(t, in.Frame("A", "test", frame(700, f2, -94), true))
	must(t, in.Frame("A", "test", frame(1800, proof, -60), true))
	if n := count(t, st, `SELECT COUNT(*) FROM packets WHERE proves_unheard = 1`); n != 0 {
		t.Errorf("proof of a heard packet flagged unheard")
	}
	if n := count(t, st, `SELECT COUNT(*) FROM frame_errors`); n != 0 {
		t.Errorf("unexpected orphans: %d", n)
	}
}

// A proof with no orphan before it is still flagged, just not linked.
func TestProofOfUnheardPacket(t *testing.T) {
	in, st := setup(t)
	_, _, proof := probeFrames(t)
	must(t, in.Frame("A", "test", frame(0, proof, -60), true))
	if n := count(t, st, `SELECT COUNT(*) FROM packets WHERE proves_unheard = 1 AND proves_orphan IS NULL`); n != 1 {
		t.Errorf("unlinked unheard proofs: %d, want 1", n)
	}
}

// In the packet feed, an orphan heard before its whole packet is absorbed:
// no orphan row, the packet row lists it, and its observations include the
// half as an O.
func TestPacketFeedAbsorbsOrphan(t *testing.T) {
	in, st := setup(t)
	f1, f2 := splitPacket(5)
	must(t, in.Frame("B", "test", frame(690, f2, -112), true)) // B: second half only
	must(t, in.Frame("A", "test", frame(0, f1, -90), true))
	must(t, in.Frame("A", "test", frame(700, f2, -91), true))

	rows, err := st.PacketFeed(t0.Add(time.Hour), time.Time{}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != "packet" || len(rows[0].Absorbed) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	r := rows[0]
	if r.Observer != "A" || r.Frames != 2 || r.Receptions != 2 || r.Partials != 1 {
		t.Errorf("summary should be A's complete reception: %+v", r)
	}
	obs, err := st.PacketObservations(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	codes := ""
	for _, o := range obs {
		codes += o.Frames
	}
	if len(obs) != 2 || codes != "OD" || obs[0].Part != "second half" {
		t.Errorf("observations: %s %+v", codes, obs)
	}
}

// An orphan group is one feed row naming every observer.
func TestPacketFeedOrphanGroup(t *testing.T) {
	in, st := setup(t)
	f1, _, proof := probeFrames(t)
	must(t, in.Frame("A", "test", frame(0, f1, -94), true))
	must(t, in.Frame("B", "test", frame(5, f1, -98), true))
	must(t, in.Frame("A", "test", frame(1300, proof, -60), true))
	must(t, in.Frame("B", "test", frame(1310, proof, -72), true))
	rows, err := st.PacketFeed(t0.Add(time.Hour), time.Time{}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var orphan, proofRow *int
	for i := range rows {
		i := i
		switch {
		case rows[i].Kind == "orphan":
			orphan = &i
		case rows[i].Type == "PROOF":
			proofRow = &i
		}
	}
	if len(rows) != 2 || orphan == nil || proofRow == nil {
		t.Fatalf("rows: %+v", rows)
	}
	o, p := rows[*orphan], rows[*proofRow]
	if o.Observers != 2 || o.LinkID != p.ID || o.Type != "DATA" {
		t.Errorf("orphan row: %+v", o)
	}
	if !p.ProvesUnheard || p.LinkID != o.ID || p.Receptions != 2 {
		t.Errorf("proof row: %+v", p)
	}
	obs, err := st.OrphanObservations(o.ID)
	if err != nil || len(obs) != 2 || obs[0].Frames != "O" {
		t.Errorf("orphan observations: %+v %v", obs, err)
	}
}
