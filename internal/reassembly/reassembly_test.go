package reassembly

import (
	"bytes"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)

func frame(ms int, header byte, n int, fill byte, rssi float32) Frame {
	d := make([]byte, 1+n)
	d[0] = header
	for i := 1; i < len(d); i++ {
		d[i] = fill
	}
	return Frame{T: t0.Add(time.Duration(ms) * time.Millisecond), RSSI: rssi, SNR: 8, HasSignal: true, Data: d}
}

func TestSingleFrame(t *testing.T) {
	r := New(9, 250000, 5)
	out := r.Feed(frame(0, 0x70, 131, 0xAA, -97))
	if len(out) != 1 || out[0].Part != Whole || len(out[0].Packet) != 131 || out[0].Header != 0x70 {
		t.Fatalf("got %+v", out)
	}
}

func TestJoinSplit(t *testing.T) {
	r := New(9, 250000, 5)
	if out := r.Feed(frame(0, 0x31, 254, 0x01, -90)); len(out) != 0 {
		t.Fatalf("first half should wait, got %+v", out)
	}
	out := r.Feed(frame(700, 0x31, 60, 0x02, -94))
	if len(out) != 1 {
		t.Fatalf("got %d results", len(out))
	}
	res := out[0]
	if res.Part != Whole || res.Frames != 2 || len(res.Packet) != 314 || res.RSSI != -92 {
		t.Fatalf("joined = part %v frames %d len %d rssi %v", res.Part, res.Frames, len(res.Packet), res.RSSI)
	}
	if !bytes.Equal(res.FrameHash[0], FrameHash(res.Packet[:254])) || !bytes.Equal(res.FrameHash[1], FrameHash(res.Packet[254:])) {
		t.Error("frame hashes do not match the halves")
	}
}

func TestOrphanFirstHalfDisplacedByNewPacket(t *testing.T) {
	r := New(9, 250000, 5)
	r.Feed(frame(0, 0x31, 254, 0x01, -90))
	out := r.Feed(frame(800, 0x50, 40, 0x03, -90)) // unsplit packet arrives instead
	if len(out) != 2 || out[0].Part != FirstHalf || out[1].Part != Whole {
		t.Fatalf("got %+v", out)
	}
	if r.Orphans != 1 {
		t.Errorf("Orphans = %d", r.Orphans)
	}
}

func TestOrphanSecondHalfAlone(t *testing.T) {
	r := New(9, 250000, 5)
	out := r.Feed(frame(0, 0x31, 60, 0x02, -90))
	if len(out) != 1 || out[0].Part != SecondHalf {
		t.Fatalf("got %+v", out)
	}
}

func TestSequenceMismatch(t *testing.T) {
	r := New(9, 250000, 5)
	r.Feed(frame(0, 0x31, 254, 0x01, -90))
	out := r.Feed(frame(700, 0x41, 60, 0x02, -90))
	if len(out) != 2 || out[0].Part != FirstHalf || out[1].Part != SecondHalf {
		t.Fatalf("got %+v", out)
	}
}

func TestTimeout(t *testing.T) {
	r := New(9, 250000, 5)
	r.Feed(frame(0, 0x31, 254, 0x01, -90))
	if out := r.Flush(t0.Add(r.Timeout / 2)); len(out) != 0 {
		t.Fatal("flushed too early")
	}
	out := r.Flush(t0.Add(r.Timeout + time.Millisecond))
	if len(out) != 1 || out[0].Part != FirstHalf {
		t.Fatalf("got %+v", out)
	}
	// A late second half with the same sequence must not join a flushed half.
	out = r.Feed(frame(5000, 0x31, 60, 0x02, -90))
	if len(out) != 1 || out[0].Part != SecondHalf {
		t.Fatalf("late half: %+v", out)
	}
}

func TestFrameAirtime(t *testing.T) {
	// SF9, 250 kHz, CR4/5, 18-symbol preamble, 255 bytes: about 0.63 s.
	at := FrameAirtime(9, 250000, 5, 18, 255)
	if at < 600*time.Millisecond || at > 660*time.Millisecond {
		t.Fatalf("airtime = %v", at)
	}
}
