package reticulum

import (
	"encoding/hex"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Examples from RFC 8949, Appendix A
func TestCBORDecodeRFCExamples(t *testing.T) {
	for _, c := range []struct {
		hex  string
		want any
	}{
		{"00", uint64(0)},
		{"17", uint64(23)},
		{"1818", uint64(24)},
		{"1903e8", uint64(1000)},
		{"1a000f4240", uint64(1000000)},
		{"1b000000e8d4a51000", uint64(1000000000000)},
		{"20", int64(-1)},
		{"3863", int64(-100)},
		{"f93c00", 1.0},
		{"f97bff", 65504.0},
		{"fa47c35000", 100000.0},
		{"fb3ff199999999999a", 1.1},
		{"f4", false},
		{"f5", true},
		{"f6", nil},
		{"6449455446", "IETF"},
		{"4401020304", []byte{1, 2, 3, 4}},
		{"8301820203820405", []any{uint64(1), []any{uint64(2), uint64(3)}, []any{uint64(4), uint64(5)}}},
		{"a26161016162820203", map[any]any{"a": uint64(1), "b": []any{uint64(2), uint64(3)}}},
		{"c11a514b67b0", uint64(1363896240)}, // tag 1 (epoch time): content kept
	} {
		got, n, err := cborDecode(unhex(t, c.hex))
		if err != nil || n != len(c.hex)/2 || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %#v (%d bytes, %v), want %#v", c.hex, got, n, err, c.want)
		}
	}
	if v, _, _ := cborDecode(unhex(t, "f97c00")); v != math.Inf(1) {
		t.Errorf("half-float infinity: %v", v)
	}
}

func TestCBORBadInput(t *testing.T) {
	for _, h := range []string{
		"",                    // nothing
		"19 03",               // truncated argument
		"65 6869",             // string longer than the data
		"9b ffffffffffffffff", // array claiming 2^64-1 items
		"bb ffffffffffffffff", // map likewise
		"9f 01 ff",            // indefinite length
		"1c",                  // reserved additional info
		"a1 8100 01",          // array as a map key
		"3b ffffffffffffffff", // negative integer below int64
	} {
		if _, _, err := cborDecode(unhex(t, h)); err == nil {
			t.Errorf("%q decoded", h)
		}
	}
	// Nesting past the depth limit
	deep := strings.Repeat("81", cborMaxDepth+2) + "01"
	if _, _, err := cborDecode(unhex(t, deep)); err == nil {
		t.Error("deep nesting decoded")
	}
	// Random bytes never panic
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		b := make([]byte, rng.Intn(40))
		rng.Read(b)
		cborDecode(b)
	}
}

// What rrcd sends: cbor2.dumps({"proto": "rrc", "v": 1, "hub": name})
func rrcAnnounce(name string) []byte {
	b := []byte{0xa3, 0x65, 'p', 'r', 'o', 't', 'o', 0x63, 'r', 'r', 'c', 0x61, 'v', 0x01, 0x63, 'h', 'u', 'b'}
	return append(append(b, byte(0x60+len(name))), name...)
}

func TestDescribeRRCHub(t *testing.T) {
	nh := NameHash("rrc.hub")
	if WellKnownName(nh) != "rrc.hub" || AppLabel(nh) != "RRC hub" {
		t.Fatalf("rrc.hub not known: %q %q", WellKnownName(nh), AppLabel(nh))
	}
	info := DescribeAnnounce(nh, rrcAnnounce("MyHub"))
	if info.Name != "MyHub" || info.Summary != `RRC hub · "MyHub" · rrc v1` {
		t.Errorf("describe: name %q, summary %q", info.Name, info.Summary)
	}
	if n := DisplayName(nh, rrcAnnounce("MyHub")); n != "MyHub" {
		t.Errorf("display name %q", n)
	}
	// Untrusted text is cleaned like any other name
	if n := DisplayName(nh, rrcAnnounce("bad\x1bname")); strings.ContainsRune(n, 0x1b) {
		t.Errorf("control character kept: %q", n)
	}
	// Garbage under the rrc.hub name: no name, and the summary says so
	if info := DescribeAnnounce(nh, []byte{0xff, 0x00}); info.Name != "" || !strings.Contains(info.Summary, "unreadable") {
		t.Errorf("garbage: %+v", info)
	}
	if info := DescribeAnnounce(nh, nil); info.Summary != "RRC hub" {
		t.Errorf("no app data: %q", info.Summary)
	}
	// The same bytes under another app name are not read as a hub
	if n := DisplayName(NameHash("other.app"), rrcAnnounce("MyHub")); n == "MyHub" {
		t.Error("CBOR read for an app that is not rrc.hub")
	}
}
