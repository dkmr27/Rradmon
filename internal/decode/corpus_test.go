package decode

import (
	"bytes"
	"os"
	"testing"

	"github.com/dkmr27/Rradmon/internal/reticulum"
)

// The testdata captures come from the bench rig (docs/spec-v0.md §2): rnsd's
// RNode on COM19 talking to a Rrptr on COM15, sniffed by the RNode on COM32.
// Every proof in them answers a packet in the same capture, so proof matching
// checks the packet hash, and rejoined split packets must hash correctly too.

func load(t *testing.T, name string) []Item {
	t.Helper()
	f, err := os.Open("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	items, err := File(f)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestCorpus1(t *testing.T) {
	items := load(t, "rig-corpus1.jsonl")
	count := map[reticulum.PacketType]int{}
	var split, proofs, matched int
	for _, it := range items {
		if it.Packet == nil {
			t.Errorf("unparsed item: part %v err %v", it.R.Part, it.Err)
			continue
		}
		count[it.Packet.Type]++
		if it.R.Frames == 2 {
			split++
			if len(it.R.Packet) <= 254 {
				t.Errorf("2-frame packet only %d bytes", len(it.R.Packet))
			}
		}
		if it.Packet.Type == reticulum.TypeProof {
			proofs++
			if it.ProofFor >= 0 {
				matched++
			}
		}
		if a := it.Announce; a != nil && a.Validity != reticulum.Valid {
			t.Errorf("announce %x: %v", it.Packet.Dest, a.Validity)
		}
	}
	want := map[reticulum.PacketType]int{
		reticulum.TypeData: 15, reticulum.TypeProof: 7,
		reticulum.TypeLinkRequest: 2, reticulum.TypeAnnounce: 6,
	}
	for k, v := range want {
		if count[k] != v {
			t.Errorf("%v: got %d, want %d", k, count[k], v)
		}
	}
	if split != 5 {
		t.Errorf("split packets: got %d, want 5 (3 x rnprobe -s 300, 2 link responses)", split)
	}
	if proofs != matched {
		t.Errorf("%d of %d proofs matched a packet", matched, proofs)
	}

	// A split probe's proof must match the rejoined packet.
	for _, it := range items {
		if it.ProofFor >= 0 && items[it.ProofFor].R.Frames == 2 {
			return
		}
	}
	t.Error("no proof matched a rejoined split packet")
}

func TestCorpus1RelayedAnnounces(t *testing.T) {
	items := load(t, "rig-corpus1.jsonl")
	// Rrptr announced three destinations; rnsd rebroadcast each with hops=1
	// and its transport ID. Original and relay must share a packet hash.
	byDest := map[string][]Item{}
	for _, it := range items {
		if it.Announce != nil {
			byDest[string(it.Packet.Dest)] = append(byDest[string(it.Packet.Dest)], it)
		}
	}
	if len(byDest) != 3 {
		t.Fatalf("announced destinations: %d, want 3", len(byDest))
	}
	for d, its := range byDest {
		if len(its) != 2 {
			t.Errorf("%x: %d copies, want 2", d, len(its))
			continue
		}
		orig, relay := its[0], its[1]
		if orig.Packet.Hops != 0 || relay.Packet.Hops != 1 || !relay.Packet.Header2 || orig.Packet.Header2 {
			t.Errorf("%x: hops %d/%d header2 %v/%v", d, orig.Packet.Hops, relay.Packet.Hops,
				orig.Packet.Header2, relay.Packet.Header2)
		}
		if !bytes.Equal(orig.Hash, relay.Hash) {
			t.Errorf("%x: relay hash differs", d)
		}
		if relay.R.T.Before(orig.R.T) {
			t.Errorf("%x: relay before original", d)
		}
	}
}

func TestNormalModeCapture(t *testing.T) {
	items := load(t, "rig-normal1.jsonl")
	var proofs, matched int
	for _, it := range items {
		if it.Packet == nil {
			t.Fatalf("unparsed: %v", it.Err)
		}
		if it.Packet.Type == reticulum.TypeProof {
			proofs++
			if it.ProofFor >= 0 {
				matched++
			}
		}
	}
	if proofs != 4 || matched != 4 {
		t.Errorf("proofs %d matched %d, want 4/4", proofs, matched)
	}
}
