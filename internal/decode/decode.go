// Package decode runs captured frames through reassembly and parsing. It is
// the offline path used by `rradmon decode` and the corpus tests, and the
// shape the live ingestor will follow.
package decode

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/dkmr27/Rradmon/internal/reassembly"
	"github.com/dkmr27/Rradmon/internal/reticulum"
)

// Record is one line of a capture file (see cmd/rradmon/capture.go).
type Record struct {
	Type    string   `json:"type"`
	T       string   `json:"t"`
	RSSI    *float32 `json:"rssi"`
	SNR     *float32 `json:"snr"`
	Raw     *bool    `json:"raw"`
	Frame   string   `json:"frame"`
	SF      uint8    `json:"sf"`
	BwHz    uint32   `json:"bw_hz"`
	CR      uint8    `json:"cr"`
	Promisc *bool    `json:"promisc"`
}

// Item is one decoded result.
type Item struct {
	R        reassembly.Result
	Packet   *reticulum.Packet // nil for orphans and unparseable packets
	Hash     []byte            // truncated packet hash, when Packet is set
	Announce *reticulum.Announce
	Err      error
	// ProofFor is set on a PROOF whose destination is the truncated hash of
	// an earlier packet in the same capture: the index of that item.
	ProofFor int
}

// File decodes a whole capture.
func File(r io.Reader) ([]Item, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var (
		ra     *reassembly.Reassembler
		items  []Item
		byHash = map[string]int{}
		raw    = true
	)
	add := func(res reassembly.Result) {
		it := Item{R: res, ProofFor: -1}
		if res.Part == reassembly.Whole {
			p, err := reticulum.Parse(res.Packet)
			it.Err = err
			if err == nil {
				it.Packet = p
				it.Hash = p.TruncatedHash()
				if p.Type == reticulum.TypeAnnounce {
					it.Announce, it.Err = reticulum.ParseAnnounce(p)
				}
				if p.Type == reticulum.TypeProof {
					if i, ok := byHash[string(p.ProvedHash())]; ok {
						it.ProofFor = i
					}
				}
				byHash[string(it.Hash)] = len(items)
			}
		}
		items = append(items, it)
	}
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		switch rec.Type {
		case "session":
			ra = reassembly.New(rec.SF, rec.BwHz, rec.CR)
			if rec.Promisc != nil {
				raw = *rec.Promisc
			}
		case "frame":
			t, err := time.Parse(time.RFC3339Nano, rec.T)
			if err != nil {
				return nil, fmt.Errorf("decode: time %q: %w", rec.T, err)
			}
			data, err := hex.DecodeString(rec.Frame)
			if err != nil {
				return nil, fmt.Errorf("decode: frame hex: %w", err)
			}
			f := reassembly.Frame{T: t, Data: data}
			if rec.RSSI != nil && rec.SNR != nil {
				f.RSSI, f.SNR, f.HasSignal = *rec.RSSI, *rec.SNR, true
			}
			isRaw := raw
			if rec.Raw != nil {
				isRaw = *rec.Raw
			}
			if !isRaw {
				// Normal mode: the RNode already joined it; no header byte.
				add(reassembly.Result{Part: reassembly.Whole, T: t, Packet: data, Frames: 1,
					RSSI: f.RSSI, SNR: f.SNR, HasSignal: f.HasSignal})
				continue
			}
			if ra == nil {
				ra = reassembly.New(9, 250000, 5)
			}
			for _, res := range ra.Feed(f) {
				add(res)
			}
		}
	}
	if ra != nil {
		for _, res := range ra.Flush(time.Now()) {
			add(res)
		}
	}
	return items, sc.Err()
}

// Describe is a one-line summary of an item.
func Describe(it Item, items []Item) string {
	r := it.R
	sig := "    no signal     "
	if r.HasSignal {
		sig = fmt.Sprintf("%6.1f dBm %5.2f dB", r.RSSI, r.SNR)
	}
	s := fmt.Sprintf("%s %s %3dB", r.T.Local().Format("15:04:05.000"), sig, len(r.Packet))
	if r.Frames == 2 {
		s += " [2 frames]"
	}
	if r.Part != reassembly.Whole {
		return s + " ORPHAN " + r.Part.String()
	}
	if it.Packet == nil {
		return s + fmt.Sprintf(" unparsed: %v", it.Err)
	}
	p := it.Packet
	s += fmt.Sprintf(" %-11s %-6s hops=%d dst=%s", p.Type, p.DestType, p.Hops, hex.EncodeToString(p.Dest[:8]))
	if p.Header2 {
		s += " via=" + hex.EncodeToString(p.TransportID[:4])
	}
	if n := reticulum.WellKnownPlain(p.Dest); n != "" {
		s += " (" + n + ")"
	}
	if a := it.Announce; a != nil {
		name := reticulum.WellKnownName(a.NameHash)
		if name == "" {
			name = "name " + hex.EncodeToString(a.NameHash)
		}
		s += fmt.Sprintf(" %s, %s", name, a.Validity)
		if !a.Emitted.IsZero() {
			s += fmt.Sprintf(", made %s before rx", r.T.Sub(a.Emitted).Round(time.Second))
		}
		if len(a.AppData) > 0 {
			s += fmt.Sprintf(", app data %dB", len(a.AppData))
		}
	} else if it.Err != nil {
		s += fmt.Sprintf(" (%v)", it.Err)
	}
	if it.ProofFor >= 0 {
		s += fmt.Sprintf(" proves %s packet %s later", items[it.ProofFor].Packet.Type,
			r.T.Sub(items[it.ProofFor].R.T).Round(time.Millisecond))
	}
	return s
}
