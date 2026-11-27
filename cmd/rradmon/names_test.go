package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/store"
)

// TestNodeNameByHand names Rrptr by hand and checks the name replaces the
// announced one everywhere, that the announced one is still reported, that
// search finds it, and that removing it brings the announced name back.
func TestNodeNameByHand(t *testing.T) {
	st := openTestStore(t)
	if _, err := importCapture(st, "../../testdata/rig-corpus1.jsonl", true); err != nil {
		t.Fatal(err)
	}
	const rrptr = "135a9914ef8e82c36ea3c82bd4689e02"
	now := time.Now().Add(time.Hour)

	if err := st.SetNodeName(rrptr, "  Bench repeater  "); err != nil {
		t.Fatal(err)
	}
	n, err := st.Node(rrptr, now)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "Bench repeater" || !n.NameManual || n.AnnouncedName != "Testing-Rrptr" {
		t.Fatalf("node: name %q manual %v announced %q", n.Name, n.NameManual, n.AnnouncedName)
	}

	found, err := st.Nodes("bench rep", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Identity != rrptr {
		t.Fatalf("search by the name set by hand found %d nodes", len(found))
	}
	if found, _ := st.Nodes("testing-rrptr", now); len(found) != 1 {
		t.Errorf("search by the announced name found %d nodes, want 1", len(found))
	}

	// The packet feed shows it for Rrptr's destinations and as transmitter
	rows, err := st.PacketFeed(now, time.Time{}, store.FeedFilter{}, 500)
	if err != nil {
		t.Fatal(err)
	}
	var dest, tx int
	for _, r := range rows {
		if r.DestName == "Testing-Rrptr" || r.TxName == "Testing-Rrptr" {
			t.Fatalf("feed still shows the announced name: %+v", r)
		}
		if r.DestName == "Bench repeater" {
			dest++
		}
		if r.TxName == "Bench repeater" {
			tx++
		}
	}
	if dest == 0 || tx == 0 {
		t.Errorf("feed rows with the name: as destination %d, as transmitter %d; want some of each", dest, tx)
	}

	if err := st.SetNodeName(rrptr, ""); err != nil {
		t.Fatal(err)
	}
	n, _ = st.Node(rrptr, now)
	if n.Name != "Testing-Rrptr" || n.NameManual || n.AnnouncedName != "" {
		t.Errorf("after removing: name %q manual %v announced %q", n.Name, n.NameManual, n.AnnouncedName)
	}
}

func TestNodeNameChecks(t *testing.T) {
	st := openTestStore(t)
	const id = "135a9914ef8e82c36ea3c82bd4689e02"
	if err := st.SetNodeName(id, strings.Repeat("é", 64)); err != nil {
		t.Errorf("64 characters: %v", err)
	}
	if err := st.SetNodeName(id, strings.Repeat("x", 65)); err == nil {
		t.Error("65 characters accepted")
	}
	if err := st.SetNodeName(id, "two\nlines"); err == nil {
		t.Error("control character accepted")
	}
	if err := st.SetNodeName("nothex", "x"); err == nil {
		t.Error("bad identity accepted")
	}
}
