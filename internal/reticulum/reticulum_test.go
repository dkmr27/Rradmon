package reticulum

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"
)

// buildAnnounce makes a signed announce the way RNS Destination.announce does.
func buildAnnounce(t *testing.T, appData []byte, header2 bool) []byte {
	t.Helper()
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := append(make([]byte, 32), edPub...) // X25519 half is not checked
	rand.Read(pub[:32])
	ih := sha256.Sum256(pub)
	nameHash := NameHash("rnstransport.probe")
	dh := sha256.Sum256(append(append([]byte{}, nameHash...), ih[:16]...))
	dest := dh[:16]
	random := make([]byte, 10)
	rand.Read(random[:5])
	ts := time.Date(2026, 11, 12, 11, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < 5; i++ {
		random[9-i] = byte(ts >> (8 * i))
	}
	signed := bytes.Join([][]byte{dest, pub, nameHash, random, appData}, nil)
	sig := ed25519.Sign(edPriv, signed)

	flags := byte(TypeAnnounce) // single, broadcast, header 1
	hdr := []byte{flags, 0}
	if header2 {
		hdr = []byte{flags | flagHeader2 | flagTransport, 1}
		hdr = append(hdr, bytes.Repeat([]byte{0xEE}, 16)...)
	}
	pkt := append(hdr, dest...)
	pkt = append(pkt, 0x00) // context
	return append(pkt, bytes.Join([][]byte{pub, nameHash, random, sig, appData}, nil)...)
}

func TestAnnounceValid(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		raw := buildAnnounce(t, []byte("node name"), h2)
		p, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if p.Header2 != h2 || p.Type != TypeAnnounce {
			t.Fatalf("parsed %v", p)
		}
		a, err := ParseAnnounce(p)
		if err != nil {
			t.Fatal(err)
		}
		if a.Validity != Valid {
			t.Errorf("header2=%v: validity %v", h2, a.Validity)
		}
		if string(a.AppData) != "node name" || WellKnownName(a.NameHash) != "rnstransport.probe" {
			t.Errorf("app data %q name %q", a.AppData, WellKnownName(a.NameHash))
		}
		if a.Emitted.UTC().Hour() != 11 {
			t.Errorf("emitted = %v", a.Emitted)
		}
	}
}

func TestAnnounceTampered(t *testing.T) {
	raw := buildAnnounce(t, []byte("node name"), false)
	raw[len(raw)-1] ^= 1 // change app data
	p, _ := Parse(raw)
	a, _ := ParseAnnounce(p)
	if a.Validity != BadSignature {
		t.Errorf("validity = %v, want bad signature", a.Validity)
	}
}

func TestHashIgnoresHopsAndTransport(t *testing.T) {
	// The same announce relayed (header 2, hops 1, transport ID) keeps its hash.
	orig := buildAnnounce(t, nil, false)
	relayed := []byte{orig[0] | flagHeader2 | flagTransport, 1}
	relayed = append(relayed, bytes.Repeat([]byte{0xAB}, 16)...)
	relayed = append(relayed, orig[2:]...)
	p1, _ := Parse(orig)
	p2, _ := Parse(relayed)
	if p1.Hash() != p2.Hash() {
		t.Error("relayed copy has a different packet hash")
	}
}

func TestPathRequestLabel(t *testing.T) {
	if WellKnownPlain(PlainDestHash("rnstransport.path.request")) != "rnstransport.path.request" {
		t.Error("path request not labelled")
	}
}

func TestShortAndIFAC(t *testing.T) {
	if _, err := Parse([]byte{0x00}); err != ErrShort {
		t.Errorf("1 byte: %v", err)
	}
	if _, err := Parse(make([]byte, 10)); err != ErrShort {
		t.Errorf("10 bytes: %v", err)
	}
	p, err := Parse(append([]byte{0x80, 2}, make([]byte, 40)...))
	if err != ErrIFAC || p == nil || p.Hops != 2 {
		t.Errorf("IFAC: %v %v", p, err)
	}
}

func TestProvedHash(t *testing.T) {
	dest := bytes.Repeat([]byte{0xAA}, 16)
	full := bytes.Repeat([]byte{0xBB}, 32)
	sig := make([]byte, 64)
	mk := func(flags, ctx byte, data []byte) *Packet {
		raw := append([]byte{flags, 0}, dest...)
		raw = append(raw, ctx)
		p, err := Parse(append(raw, data...))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	proofSingle := byte(TypeProof)                   // single destination
	proofLink := byte(TypeProof) | byte(DestLink)<<2 // link destination
	cases := []struct {
		name string
		p    *Packet
		want []byte
	}{
		{"explicit proof (probe)", mk(proofSingle, 0, sig), dest},
		{"link request proof", mk(proofLink, ContextLRProof, sig), dest},
		{"link packet proof", mk(proofLink, 0, append(append([]byte{}, full...), sig...)), full[:16]},
		{"resource proof", mk(proofLink, ContextResourceProof, append(append([]byte{}, full...), sig...)), nil},
		{"short link proof", mk(proofLink, 0, sig), nil},
		{"not a proof", mk(0, 0, sig), nil},
	}
	for _, c := range cases {
		if got := c.p.ProvedHash(); !bytes.Equal(got, c.want) {
			t.Errorf("%s: got %x, want %x", c.name, got, c.want)
		}
	}
}
