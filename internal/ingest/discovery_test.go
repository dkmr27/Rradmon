package ingest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/reticulum"
)

// signedAnnounce builds a valid announce for `app` with app data, as one raw
// RNode frame (header byte included). Returns the frame and the identity hash.
func signedAnnounce(t *testing.T, app string, appData []byte) ([]byte, []byte) {
	t.Helper()
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	pub := append(make([]byte, 32), edPub...)
	rand.Read(pub[:32])
	ih := sha256.Sum256(pub)
	nameHash := reticulum.NameHash(app)
	dh := sha256.Sum256(append(append([]byte{}, nameHash...), ih[:16]...))
	dest := dh[:16]
	random := make([]byte, 10)
	rand.Read(random)
	signed := bytes.Join([][]byte{dest, pub, nameHash, random, appData}, nil)
	sig := ed25519.Sign(edPriv, signed)
	pkt := append([]byte{byte(reticulum.TypeAnnounce), 0}, dest...)
	pkt = append(pkt, 0)
	pkt = append(pkt, bytes.Join([][]byte{pub, nameHash, random, sig, appData}, nil)...)
	return append([]byte{0x20}, pkt...), ih[:16]
}

// Discovery app data with a made-up position: flags, map, 32-byte stamp.
func discoveryData() []byte {
	var b bytes.Buffer
	b.WriteByte(0x00)           // flags: not encrypted
	b.WriteByte(0x84)           // map of 4
	b.Write([]byte{0xcc, 0xFF}) // key 255 (name), as uint8
	b.Write([]byte{0xa7, 'H', 'i', 'l', 'l', 't', 'o', 'p'})
	b.WriteByte(0x00)
	b.Write([]byte{0xae, 'R', 'N', 'o', 'd', 'e', 'I', 'n', 't', 'e', 'r', 'f', 'a', 'c', 'e'})
	f64 := func(f float64) {
		b.WriteByte(0xcb)
		binary.Write(&b, binary.BigEndian, math.Float64bits(f))
	}
	b.WriteByte(0x03)
	f64(-10.5)
	b.WriteByte(0x04)
	f64(120.25)
	b.Write(make([]byte, 32)) // stamp
	return b.Bytes()
}

func TestDiscoveryPlacesNode(t *testing.T) {
	in, st := setup(t)
	frameData, ident := signedAnnounce(t, "rnstransport.discovery.interface", discoveryData())
	if len(frameData)-1 > 254 {
		t.Fatalf("test announce too long for one frame: %d", len(frameData))
	}
	must(t, in.Frame("A", "test", frame(0, frameData, -80), true))
	nomad, _ := signedAnnounce(t, "nomadnetwork.node", []byte("Garden Node"))
	must(t, in.Frame("A", "test", frame(2000, nomad, -85), true))

	md, err := st.Map(t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Nodes) != 1 || md.Unplaced != 1 {
		t.Fatalf("map: %d placed, %d unplaced (want 1, 1)", len(md.Nodes), md.Unplaced)
	}
	n := md.Nodes[0]
	if n.Lat != -10.5 || n.Lon != 120.25 || n.PosSource != "discovery" || n.Name != "Hilltop" {
		t.Errorf("placed node: %+v", n)
	}
	if n.Identity != hexs(ident) {
		t.Errorf("identity %s, want %s", n.Identity, hexs(ident))
	}

	rows, err := st.PacketFeed(t0.Add(time.Hour), time.Time{}, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	infos := map[string]string{}
	for _, r := range rows {
		infos[r.AppName] = r.Info
	}
	if !strings.HasPrefix(infos["rnstransport.discovery.interface"], `"Hilltop" · RNodeInterface`) ||
		!strings.Contains(infos["rnstransport.discovery.interface"], "position") {
		t.Errorf("discovery info: %q", infos["rnstransport.discovery.interface"])
	}
	if infos["nomadnetwork.node"] != `"Garden Node"` {
		t.Errorf("nomadnet info: %q", infos["nomadnetwork.node"])
	}
}

func hexs(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, h[c>>4], h[c&15])
	}
	return string(out)
}
