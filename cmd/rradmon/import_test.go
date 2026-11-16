package main

import (
	"encoding/hex"
	"testing"

	"github.com/dkmr27/Rradmon/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func count(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestImportCorpus1 stores the bench capture and checks dedupe, announces,
// destinations and transmitter attribution. Rrptr's transport identity is
// 135a9914… (the hash used with rnstatus -R); rnsd's is 24de22aa….
func TestImportCorpus1(t *testing.T) {
	st := openTestStore(t)
	if _, err := importCapture(st, "../../testdata/rig-corpus1.jsonl", true); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		q    string
		want int
	}{
		{"packets (3 relayed announces deduped)", `SELECT COUNT(*) FROM packets`, 27},
		{"receptions", `SELECT COUNT(*) FROM receptions`, 30},
		{"repeat receptions", `SELECT COUNT(*) FROM receptions WHERE delay_ms > 0`, 3},
		{"announces", `SELECT COUNT(*) FROM announces`, 3},
		{"destinations", `SELECT COUNT(*) FROM destinations`, 3},
		{"split packets with frame hashes", `SELECT COUNT(*) FROM packets WHERE h1 IS NOT NULL AND h2 IS NOT NULL`, 5},
		{"frame errors", `SELECT COUNT(*) FROM frame_errors`, 0},
		{"channel reports (318 averaged per minute)", `SELECT COUNT(*) FROM channel_stats`, 5},
		{"NomadNet name", `SELECT COUNT(*) FROM destinations WHERE display_name = 'Testing-Rrptr'`, 1},
	}
	for _, c := range checks {
		if got := count(t, st, c.q); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}

	rrptr, _ := hex.DecodeString("135a9914ef8e82c36ea3c82bd4689e02")
	if n := count(t, st, `SELECT COUNT(*) FROM receptions WHERE tx_identity = ?`, rrptr); n != 3 {
		t.Errorf("receptions attributed to Rrptr: %d, want 3", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM destinations WHERE identity_hash = ?`, rrptr); n != 3 {
		t.Errorf("Rrptr destinations: %d, want 3", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM receptions WHERE hex(tx_identity) LIKE '24DE22AA%'`); n != 3 {
		t.Errorf("receptions attributed to rnsd: %d, want 3", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM node_stats_hourly`); n != 2 {
		t.Errorf("node stats rows: %d, want 2 (Rrptr, rnsd)", n)
	}
}

// TestProofAttribution imports the capture twice into one database, shifted
// in time by the second import's own records being new: once the probe
// destination's identity is known from its announce, proofs from that node
// are attributed to it.
func TestProofAttribution(t *testing.T) {
	st := openTestStore(t)
	if _, err := importCapture(st, "../../testdata/rig-corpus1.jsonl", true); err != nil {
		t.Fatal(err)
	}
	// rig-normal1 was captured earlier in the day, but its packets are new
	// and the destinations are known now.
	if _, err := importCapture(st, "../../testdata/rig-normal1.jsonl", true); err != nil {
		t.Fatal(err)
	}
	rrptr, _ := hex.DecodeString("135a9914ef8e82c36ea3c82bd4689e02")
	// 4 probe proofs in rig-normal1, sent directly by Rrptr.
	if n := count(t, st, `SELECT COUNT(*) FROM receptions r JOIN packets p ON p.id = r.packet_id
		WHERE p.pkt_type = 3 AND r.tx_identity = ?`, rrptr); n != 4 {
		t.Errorf("proofs attributed to Rrptr: %d, want 4", n)
	}
}
