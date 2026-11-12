package kiss

import (
	"bytes"
	"testing"
)

func TestEncodeEscapes(t *testing.T) {
	got := Encode(0x00, []byte{0x01, FEND, 0x02, FESC, 0x03})
	want := []byte{FEND, 0x00, 0x01, FESC, TFEND, 0x02, FESC, TFESC, 0x03, FEND}
	if !bytes.Equal(got, want) {
		t.Fatalf("Encode = % x, want % x", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	var d Decoder
	frames := d.Feed(Encode(0x00, data))
	if len(frames) != 1 || frames[0].Cmd != 0x00 || !bytes.Equal(frames[0].Data, data) {
		t.Fatalf("round trip failed: %+v", frames)
	}
}

func TestSplitAcrossFeeds(t *testing.T) {
	stream := append(Encode(0x23, []byte{0x40}), Encode(0x24, []byte{FEND})...)
	var d Decoder
	var frames []Frame
	for _, b := range stream {
		frames = append(frames, d.Feed([]byte{b})...)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if frames[0].Cmd != 0x23 || frames[0].Data[0] != 0x40 {
		t.Errorf("frame 0 = %+v", frames[0])
	}
	if frames[1].Cmd != 0x24 || frames[1].Data[0] != FEND {
		t.Errorf("frame 1 = %+v", frames[1])
	}
}

func TestIgnoresNoiseAndEmptyFrames(t *testing.T) {
	var d Decoder
	// Boot noise before the first FEND, then back-to-back FENDs.
	stream := []byte("ESP-ROM:boot\r\n")
	stream = append(stream, FEND, FEND, FEND)
	stream = append(stream, Encode(0x08, []byte{0x46})...)
	frames := d.Feed(stream)
	if len(frames) != 1 || frames[0].Cmd != 0x08 || frames[0].Data[0] != 0x46 {
		t.Fatalf("got %+v", frames)
	}
}

func TestOversizeDropped(t *testing.T) {
	var d Decoder
	big := make([]byte, MaxFrame+10)
	frames := d.Feed(append([]byte{FEND, 0x00}, big...))
	frames = append(frames, d.Feed(Encode(0x23, []byte{0x50}))...)
	if d.Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", d.Dropped)
	}
	if len(frames) != 1 || frames[0].Cmd != 0x23 {
		t.Fatalf("recovery failed: %+v", frames)
	}
}
