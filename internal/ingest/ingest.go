// Package ingest turns frames from any source into stored packets:
// per-observer reassembly, parsing, announce validation, dedupe and storage.
// Both the RNode serial source and (later) MQTT observers feed it.
package ingest

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dkmr27/Rradmon/internal/reassembly"
	"github.com/dkmr27/Rradmon/internal/reticulum"
	"github.com/dkmr27/Rradmon/internal/store"
)

// Radio is the network's LoRa setting.
type Radio struct {
	FreqHz, BandwidthHz uint32
	SF, CR              uint8
}

// Slug names the radio profile, e.g. "925875000-250000-9-5".
func (r Radio) Slug() string {
	return fmt.Sprintf("%d-%d-%d-%d", r.FreqHz, r.BandwidthHz, r.SF, r.CR)
}

// Event describes one stored result, for the live view and logs.
type Event struct {
	Observer string
	T        time.Time
	Item     Item
	Rec      store.Recorded // for whole packets
	Matched  int64          // for orphans: packet id matched, or 0
}

// Item is a reassembly result plus whatever parsing made of it.
type Item struct {
	R        reassembly.Result
	Packet   *reticulum.Packet
	Announce *reticulum.Announce
	Err      error
}

// Ingestor is safe for concurrent use by several sources.
type Ingestor struct {
	st      *store.Store
	radio   Radio
	network int64
	log     *slog.Logger

	// OnEvent, if set, is called after each item is stored.
	OnEvent func(Event)

	mu        sync.Mutex
	observers map[string]*observer
}

type observer struct {
	id      int64
	ext     string
	reas    *reassembly.Reassembler
	touched time.Time
}

// New prepares an ingestor for one radio network.
func New(st *store.Store, radio Radio, log *slog.Logger) (*Ingestor, error) {
	if log == nil {
		log = slog.Default()
	}
	id, err := st.Network(radio.Slug(), radio.FreqHz, radio.BandwidthHz, radio.SF, radio.CR)
	if err != nil {
		return nil, err
	}
	return &Ingestor{st: st, radio: radio, network: id, log: log, observers: map[string]*observer{}}, nil
}

// touchEvery limits how often an observer's last_seen is written.
const touchEvery = 30 * time.Second

func (in *Ingestor) observer(ext, source string, t time.Time) (*observer, error) {
	if o, ok := in.observers[ext]; ok {
		if t.Sub(o.touched) >= touchEvery {
			o.touched = t
			if err := in.st.TouchObserver(ext, t); err != nil {
				return nil, err
			}
		}
		return o, nil
	}
	id, err := in.st.Observer(ext, source, t)
	if err != nil {
		return nil, err
	}
	o := &observer{id: id, ext: ext, touched: t, reas: reassembly.New(in.radio.SF, in.radio.BandwidthHz, in.radio.CR)}
	in.observers[ext] = o
	return o, nil
}

// Frame ingests one frame from an observer. raw says whether it carries the
// RNode header byte (promiscuous mode) or is already a whole packet.
func (in *Ingestor) Frame(ext, source string, f reassembly.Frame, raw bool) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	o, err := in.observer(ext, source, f.T)
	if err != nil {
		return err
	}
	if !raw {
		return in.item(o, reassembly.Result{Part: reassembly.Whole, T: f.T, Packet: f.Data, Frames: 1,
			RSSI: f.RSSI, SNR: f.SNR, HasSignal: f.HasSignal})
	}
	for _, res := range o.reas.Feed(f) {
		if err := in.item(o, res); err != nil {
			return err
		}
	}
	return nil
}

// Flush stores any first halves that have waited too long. Call it about once
// a second.
func (in *Ingestor) Flush(now time.Time) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, o := range in.observers {
		for _, res := range o.reas.Flush(now) {
			if err := in.item(o, res); err != nil {
				return err
			}
		}
	}
	return nil
}

// Channel stores a channel report from an observer.
func (in *Ingestor) Channel(ext, source string, c store.ChannelStats) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	o, err := in.observer(ext, source, c.T)
	if err != nil {
		return err
	}
	return in.st.AddChannelStats(o.id, c)
}

func (in *Ingestor) item(o *observer, res reassembly.Result) error {
	it := Item{R: res}
	var rssi, snr *float32
	if res.HasSignal {
		r, s := res.RSSI, res.SNR
		rssi, snr = &r, &s
	}
	ev := Event{Observer: o.ext, T: res.T}

	if res.Part == reassembly.Whole {
		p, err := reticulum.Parse(res.Packet)
		if err == nil && p.Type == reticulum.TypeAnnounce {
			it.Announce, err = reticulum.ParseAnnounce(p)
		}
		it.Packet, it.Err = p, err
		if err == nil {
			rec, err := in.st.RecordReception(store.Reception{
				NetworkID: in.network, ObserverID: o.id, T: res.T, RSSI: rssi, SNR: snr,
				Packet: p, Announce: it.Announce, Frames: res.Frames, FrameHash: res.FrameHash,
				StorePayload:   in.st.SettingBool("ingest.store_payloads"),
				WindowAnnounce: in.st.SettingDuration("ingest.window_announce"),
				WindowOther:    in.st.SettingDuration("ingest.window_other"),
			})
			if err != nil {
				return err
			}
			ev.Item, ev.Rec = it, rec
			in.emit(ev)
			return nil
		}
		// Unparseable (short, IFAC, bad announce): keep as a frame error.
	}

	kind := res.Part.String()
	if res.Part == reassembly.Whole {
		kind = "unparsed: " + it.Err.Error()
	}
	hdr := res.Header
	orph := store.Orphan{
		NetworkID: in.network, ObserverID: o.id, T: res.T, RSSI: rssi, SNR: snr,
		Kind: kind, Header: &hdr, Len: len(res.Packet),
		Window: in.st.SettingDuration("ingest.window_other"),
	}
	if res.Part == reassembly.FirstHalf || res.Part == reassembly.SecondHalf {
		orph.FrameHash = res.FrameHash[0]
	}
	if res.Part == reassembly.FirstHalf || res.Part == reassembly.Whole {
		orph.Head = res.Packet[:min(len(res.Packet), 2+2*reticulum.HashLen+1)]
	}
	matched, err := in.st.RecordOrphan(orph)
	if err != nil {
		return err
	}
	ev.Item, ev.Matched = it, matched
	in.emit(ev)
	return nil
}

func (in *Ingestor) emit(ev Event) {
	if in.OnEvent != nil {
		in.OnEvent(ev)
	}
}
