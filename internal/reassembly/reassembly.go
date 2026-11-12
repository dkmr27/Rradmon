// Package reassembly joins RNode split frames back into Reticulum packets,
// one Reassembler per observer. See docs/spec-v0.md §1.
//
// Every LoRa frame starts with a 1-byte RNode header: a random sequence in the
// high nibble and a split flag in bit 0. A packet over 254 bytes is sent as two
// frames with the same header byte, 254 bytes then the rest.
package reassembly

import (
	"crypto/sha256"
	"math"
	"time"
)

const (
	headerSplit = 0x01
	headerSeq   = 0xF0
	// FramePayload is the packet bytes carried by the first frame of a split.
	FramePayload = 254
	// MTU is the largest packet two frames can carry.
	MTU = 508
)

// Part says which piece of a transmission a Result holds.
type Part uint8

const (
	Whole       Part = 0 // a single-frame packet or a joined split packet
	FirstHalf   Part = 1 // orphaned first frame of a split packet (header readable)
	SecondHalf  Part = 2 // orphaned second frame (opaque)
	UnknownHalf Part = 3 // orphaned split frame that could be either
	ShortFrame  Part = 4 // frame with no packet bytes
)

func (p Part) String() string {
	return [...]string{"whole", "first half", "second half", "unknown half", "short frame"}[p]
}

// Frame is one raw LoRa frame from an observer.
type Frame struct {
	T         time.Time
	RSSI, SNR float32
	HasSignal bool
	Data      []byte // including the RNode header byte
}

// Result is a packet, or a piece of one, ready for parsing.
type Result struct {
	Part      Part
	T         time.Time // time of the last frame
	Header    byte      // RNode header byte
	Packet    []byte    // Reticulum bytes (a half's bytes for orphans)
	Frames    int
	RSSI, SNR float32 // mean over the frames when joined, as RNode does
	HasSignal bool
	Signal    []Signal // per frame
	// FrameHash holds a 16-byte hash of each frame's packet bytes, for split
	// packets and orphans, so an orphan can be matched to the whole packet
	// heard elsewhere (spec §1, partial receptions).
	FrameHash [][]byte
}

// Signal is one frame's RSSI/SNR.
type Signal struct {
	RSSI, SNR float32
	HasSignal bool
}

// FrameHash is the hash used to match orphaned halves.
func FrameHash(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:16]
}

// Reassembler holds at most one waiting first half.
type Reassembler struct {
	// Timeout is how long a first half waits for its second.
	Timeout time.Duration

	pending *Frame
	Orphans int // counter for health stats
}

// New returns a Reassembler whose timeout fits the radio setting: twice the
// airtime of a full frame, plus slack for serial and scheduling delays.
func New(sf uint8, bwHz uint32, cr uint8) *Reassembler {
	at := FrameAirtime(sf, bwHz, cr, 18, 1+FramePayload)
	return &Reassembler{Timeout: 2*at + 500*time.Millisecond}
}

// Feed takes the next frame and returns any results it completes. Results
// come out in order; an orphan comes out before the frame that displaced it.
func (r *Reassembler) Feed(f Frame) []Result {
	var out []Result
	if r.pending != nil && f.T.Sub(r.pending.T) > r.Timeout {
		out = append(out, r.orphan(*r.pending))
		r.pending = nil
	}
	if len(f.Data) < 2 {
		return append(out, Result{Part: ShortFrame, T: f.T, Frames: 1, Packet: f.Data[min(1, len(f.Data)):],
			RSSI: f.RSSI, SNR: f.SNR, HasSignal: f.HasSignal, Signal: []Signal{sig(f)}})
	}
	h := f.Data[0]
	payload := f.Data[1:]

	if h&headerSplit == 0 {
		if r.pending != nil {
			out = append(out, r.orphan(*r.pending))
			r.pending = nil
		}
		return append(out, Result{Part: Whole, T: f.T, Header: h, Packet: payload, Frames: 1,
			RSSI: f.RSSI, SNR: f.SNR, HasSignal: f.HasSignal, Signal: []Signal{sig(f)}})
	}

	if r.pending != nil && r.pending.Data[0]&headerSeq == h&headerSeq && len(r.pending.Data)-1+len(payload) <= MTU {
		first := *r.pending
		r.pending = nil
		return append(out, join(first, f))
	}

	if r.pending != nil {
		out = append(out, r.orphan(*r.pending))
		r.pending = nil
	}
	if len(payload) < FramePayload {
		// Only a second half can be shorter than a full frame.
		return append(out, r.orphan(f))
	}
	cp := f
	r.pending = &cp
	return out
}

// Flush returns a waiting first half as an orphan if it has timed out by now.
func (r *Reassembler) Flush(now time.Time) []Result {
	if r.pending == nil || now.Sub(r.pending.T) <= r.Timeout {
		return nil
	}
	res := r.orphan(*r.pending)
	r.pending = nil
	return []Result{res}
}

func sig(f Frame) Signal { return Signal{f.RSSI, f.SNR, f.HasSignal} }

func join(a, b Frame) Result {
	p1, p2 := a.Data[1:], b.Data[1:]
	pkt := make([]byte, 0, len(p1)+len(p2))
	pkt = append(append(pkt, p1...), p2...)
	res := Result{
		Part: Whole, T: b.T, Header: a.Data[0], Packet: pkt, Frames: 2,
		Signal:    []Signal{sig(a), sig(b)},
		FrameHash: [][]byte{FrameHash(p1), FrameHash(p2)},
	}
	switch {
	case a.HasSignal && b.HasSignal:
		res.RSSI, res.SNR, res.HasSignal = (a.RSSI+b.RSSI)/2, (a.SNR+b.SNR)/2, true
	case a.HasSignal:
		res.RSSI, res.SNR, res.HasSignal = a.RSSI, a.SNR, true
	case b.HasSignal:
		res.RSSI, res.SNR, res.HasSignal = b.RSSI, b.SNR, true
	}
	return res
}

func (r *Reassembler) orphan(f Frame) Result {
	r.Orphans++
	payload := f.Data[1:]
	part := SecondHalf
	if len(payload) == FramePayload {
		// A full-size frame is normally a first half, but a second half can
		// also be exactly 254 bytes (a 508-byte packet).
		part = FirstHalf
	}
	return Result{Part: part, T: f.T, Header: f.Data[0], Packet: payload, Frames: 1,
		RSSI: f.RSSI, SNR: f.SNR, HasSignal: f.HasSignal, Signal: []Signal{sig(f)},
		FrameHash: [][]byte{FrameHash(payload)}}
}

// FrameAirtime is the LoRa time on air for n payload bytes with an explicit
// header and CRC on (Semtech AN1200.13).
func FrameAirtime(sf uint8, bwHz uint32, cr uint8, preamble int, n int) time.Duration {
	ts := math.Pow(2, float64(sf)) / float64(bwHz) // seconds per symbol
	de := 0.0
	if ts > 0.016 {
		de = 1 // low data rate optimisation
	}
	crIdx := float64(cr) - 4 // 4/5 -> 1
	num := 8*float64(n) - 4*float64(sf) + 28 + 16
	den := 4 * (float64(sf) - 2*de)
	payloadSym := 8 + math.Max(math.Ceil(num/den)*(crIdx+4), 0)
	total := (float64(preamble) + 4.25 + payloadSym) * ts
	return time.Duration(total * float64(time.Second))
}
