package main

import (
	"errors"
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/store"
)

// TestDeleteNode deletes Rrptr from a capture, first keeping its traffic and
// then (on a fresh copy) with its packets, and checks what goes and what
// stays, and that the database stays consistent.
func TestDeleteNode(t *testing.T) {
	const rrptr = "135a9914ef8e82c36ea3c82bd4689e02"
	now := time.Now().Add(time.Hour)
	count := func(st *store.Store, q string) int {
		var n int
		if err := st.DB.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	check := func(st *store.Store) {
		t.Helper()
		if n := count(st, `SELECT COUNT(*) FROM pragma_foreign_key_check`); n != 0 {
			t.Errorf("%d foreign key errors", n)
		}
		if n := count(st, `SELECT COUNT(*) FROM receptions WHERE packet_id NOT IN (SELECT id FROM packets)`); n != 0 {
			t.Errorf("%d receptions without a packet", n)
		}
		if _, err := st.Node(rrptr, now); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("node page after delete: %v, want not found", err)
		}
		nodes, err := st.Nodes("", now)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if n.Identity == rrptr {
				t.Errorf("node still listed: %+v", n)
			}
		}
	}
	load := func() *store.Store {
		st := openTestStore(t)
		if _, err := importCapture(st, "../../testdata/rig-corpus1.jsonl", true); err != nil {
			t.Fatal(err)
		}
		if err := st.SetNodeName(rrptr, "Bench repeater"); err != nil {
			t.Fatal(err)
		}
		if err := st.SetPin(rrptr, store.Pin{Lat: -33.5, Lon: 151.25}); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Keeping its traffic
	st := load()
	packets := count(st, `SELECT COUNT(*) FROM packets`)
	receptions := count(st, `SELECT COUNT(*) FROM receptions`)
	sent := count(st, `SELECT COUNT(*) FROM packets WHERE tx_identity = X'`+rrptr+`'`)
	if sent == 0 {
		t.Fatal("test capture has no packets from Rrptr")
	}
	res, err := st.DeleteNode(rrptr, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Destinations < 1 || res.Packets != 0 {
		t.Errorf("result %+v", res)
	}
	check(st)
	if n := count(st, `SELECT COUNT(*) FROM packets`); n != packets {
		t.Errorf("packets %d, want all %d kept", n, packets)
	}
	if n := count(st, `SELECT COUNT(*) FROM receptions`); n != receptions {
		t.Errorf("receptions %d, want all %d kept", n, receptions)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM node_names`, `SELECT COUNT(*) FROM node_pins`,
		`SELECT COUNT(*) FROM node_stats_hourly WHERE tx_identity = X'` + rrptr + `'`,
		`SELECT COUNT(*) FROM packets WHERE tx_identity = X'` + rrptr + `'`,
	} {
		if n := count(st, q); n != 0 {
			t.Errorf("%s: %d left", q, n)
		}
	}
	if _, err := st.DeleteNode(rrptr, false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting it again: %v, want not found", err)
	}

	// With its packets
	st = load()
	res, err = st.DeleteNode(rrptr, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Packets < int64(sent) {
		t.Errorf("deleted %d packets, want at least the %d it sent", res.Packets, sent)
	}
	check(st)
	if n := count(st, `SELECT COUNT(*) FROM packets`); n != packets-int(res.Packets) {
		t.Errorf("packets %d, want %d", n, packets-int(res.Packets))
	}
	if n := count(st, `SELECT COUNT(*) FROM announces WHERE identity_hash = X'`+rrptr+`'`); n != 0 {
		t.Errorf("%d of its announces left", n)
	}
}
