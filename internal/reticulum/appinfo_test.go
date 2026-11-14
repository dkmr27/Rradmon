package reticulum

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"strings"
	"testing"
)

// A tiny msgpack encoder for test data.
type mp struct{ bytes.Buffer }

func (m *mp) mapHeader(n int)   { m.WriteByte(0x80 | byte(n)) }
func (m *mp) arrayHeader(n int) { m.WriteByte(0x90 | byte(n)) }
func (m *mp) uint(v uint64) {
	switch {
	case v < 128:
		m.WriteByte(byte(v))
	case v < 1<<32:
		m.WriteByte(0xce)
		binary.Write(m, binary.BigEndian, uint32(v))
	default:
		m.WriteByte(0xcf)
		binary.Write(m, binary.BigEndian, v)
	}
}
func (m *mp) str(s string) {
	m.WriteByte(0xd9)
	m.WriteByte(byte(len(s)))
	m.WriteString(s)
}
func (m *mp) bin(b []byte) {
	m.WriteByte(0xc4)
	m.WriteByte(byte(len(b)))
	m.Write(b)
}
func (m *mp) f64(f float64) {
	m.WriteByte(0xcb)
	binary.Write(m, binary.BigEndian, math.Float64bits(f))
}
func (m *mp) boolean(b bool) {
	if b {
		m.WriteByte(0xc3)
	} else {
		m.WriteByte(0xc2)
	}
}

// discoveryAppData builds discovery app data the way RNS Discovery.py does.
// The coordinates are made up.
func discoveryAppData(encrypted bool) []byte {
	var m mp
	m.mapHeader(12)
	m.uint(0x00)
	m.str("RNodeInterface")
	m.uint(0x01)
	m.boolean(true)
	m.uint(0xFE)
	m.bin(bytes.Repeat([]byte{0x13}, 16))
	m.uint(0xFD)
	m.str("Rrptr")
	m.uint(0xFC)
	m.str("0.1.0")
	m.uint(0xFF)
	m.str("Hilltop")
	m.uint(0x03)
	m.f64(-10.5)
	m.uint(0x04)
	m.f64(120.25)
	m.uint(0x05)
	m.f64(312)
	m.uint(0x09)
	m.uint(925875000)
	m.uint(0x0A)
	m.uint(250000)
	m.uint(0x0B)
	m.uint(9)
	flags := byte(0)
	if encrypted {
		flags = 0x02
	}
	out := append([]byte{flags}, m.Bytes()...)
	return append(out, make([]byte, discoveryStampLen)...) // stamp
}

func TestDescribeDiscovery(t *testing.T) {
	info := DescribeAnnounce(NameHash("rnstransport.discovery.interface"), discoveryAppData(false))
	d := info.Discovery
	if d == nil || d.Name != "Hilltop" || info.Name != "Hilltop" || d.Interface != "RNodeInterface" ||
		d.Impl != "Rrptr" || d.Version != "0.1.0" || !d.Transport || len(d.TransportID) != 16 ||
		d.FreqHz != 925875000 || d.BwHz != 250000 || d.SF != 9 {
		t.Fatalf("parsed %+v", d)
	}
	if d.Lat == nil || *d.Lat != -10.5 || *d.Lon != 120.25 || *d.Height != 312 {
		t.Errorf("position %v %v %v", d.Lat, d.Lon, d.Height)
	}
	// Type first; no interface type; the (public, broadcast) position in full
	want := `Discovery · "Hilltop" · Rrptr 0.1.0 · 925.875 MHz 250 kHz SF9 · -10.50000, 120.25000 · 312 m`
	if info.Summary != want {
		t.Errorf("summary\n got %s\nwant %s", info.Summary, want)
	}
}

func TestDescribeDiscoveryEncrypted(t *testing.T) {
	info := DescribeAnnounce(NameHash("rnstransport.discovery.interface"), discoveryAppData(true))
	if info.Discovery == nil || !info.Discovery.Encrypted || info.Discovery.Lat != nil || info.Name != "" {
		t.Errorf("encrypted: %+v", info)
	}
}

func TestDescribeLXMFAndNomad(t *testing.T) {
	var m mp
	m.arrayHeader(2)
	m.bin([]byte("Alice"))
	m.uint(8)
	info := DescribeAnnounce(NameHash("lxmf.delivery"), m.Bytes())
	if info.Name != "Alice" || info.Summary != `LXMF · "Alice" · stamp cost 8` {
		t.Errorf("lxmf: %+v", info)
	}
	info = DescribeAnnounce(NameHash("nomadnetwork.node"), []byte("Testing-Rrptr"))
	if info.Name != "Testing-Rrptr" || info.Summary != `NomadNet · "Testing-Rrptr"` {
		t.Errorf("nomadnet: %+v", info)
	}
	info = DescribeAnnounce(NameHash("some.app"), []byte{0x00, 0xff, 0x10})
	if info.Name != "" || info.Summary != "Announce · 3 bytes of app data 00ff10" {
		t.Errorf("binary: %+v", info)
	}
	if info := DescribeAnnounce(NameHash("rnstransport.probe"), nil); info.Summary != "Probe" {
		t.Errorf("empty: %+v", info)
	}
}

// App data is untrusted: no input may panic or hang the decoder.
func TestMsgpackGarbage(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	disc := NameHash("rnstransport.discovery.interface")
	good := discoveryAppData(false)
	for i := 0; i < 20000; i++ {
		var b []byte
		if i%2 == 0 {
			b = make([]byte, rnd.Intn(200))
			rnd.Read(b)
		} else { // mutate a valid one
			b = append([]byte(nil), good...)
			for k := 0; k < 1+rnd.Intn(4); k++ {
				b[rnd.Intn(len(b))] = byte(rnd.Intn(256))
			}
			b = b[:rnd.Intn(len(b)+1)]
		}
		DescribeAnnounce(disc, b)
		DescribeAnnounce(NameHash("lxmf.delivery"), b)
	}
	// Huge declared lengths fail cleanly
	for _, b := range [][]byte{{0xdd, 0xff, 0xff, 0xff, 0xff}, {0xdf, 0xff, 0xff, 0xff, 0xff}, {0xc6, 0xff, 0xff, 0xff, 0xff}} {
		if _, _, err := msgpackDecode(b); err == nil {
			t.Errorf("% x: no error", b)
		}
	}
}

// Untrusted text from app data stays short, on one line and upright.
func TestCleanUntrustedText(t *testing.T) {
	cases := map[string]string{
		"Garden\nNode\t\t2": "Garden Node 2",
		"evil‮edoc":         "eviledoc", // right-to-left override removed
		"zero​width":        "zerowidth",
		"  spaced   out  ":  "spaced out",
		"Ź̂̃̄̅":            "Ź̂", // at most 2 combining marks
	}
	for in, want := range cases {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 500)
	if got := clean(long); len([]rune(got)) != MaxTextRunes+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("long text: %d runes", len([]rune(got)))
	}
	// A NomadNet name of 400 printable characters is capped in the summary
	info := DescribeAnnounce(NameHash("nomadnetwork.node"), []byte(strings.Repeat("x", 400)))
	if len([]rune(info.Summary)) > len("NomadNet · ")+MaxTextRunes+3 {
		t.Errorf("summary too long: %d runes", len([]rune(info.Summary)))
	}
}

func TestDescribeDiscoveryZeroPosition(t *testing.T) {
	// 0,0 is how an unset position is sent: no position, and none in the summary
	ad := discoveryAppData(false)
	zero := bytes.Replace(ad, binF64(-10.5), binF64(0), 1)
	zero = bytes.Replace(zero, binF64(120.25), binF64(0), 1)
	info := DescribeAnnounce(NameHash("rnstransport.discovery.interface"), zero)
	if d := info.Discovery; d == nil || d.Lat != nil || d.Lon != nil || d.Height != nil {
		t.Fatalf("position not dropped: %+v", info.Discovery)
	}
	if strings.Contains(info.Summary, "0.00000") || strings.Contains(info.Summary, " m") {
		t.Errorf("summary shows a position: %s", info.Summary)
	}
}

func binF64(v float64) []byte {
	var m mp
	m.f64(v)
	return m.Bytes()
}
