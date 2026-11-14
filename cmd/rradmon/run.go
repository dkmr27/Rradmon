package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/dkmr27/Rradmon/internal/decode"
	"github.com/dkmr27/Rradmon/internal/ingest"
	"github.com/dkmr27/Rradmon/internal/mqtt"
	"github.com/dkmr27/Rradmon/internal/reassembly"
	"github.com/dkmr27/Rradmon/internal/reticulum"
	"github.com/dkmr27/Rradmon/internal/rnode"
	"github.com/dkmr27/Rradmon/internal/store"
	"github.com/dkmr27/Rradmon/internal/web"
)

// runRun reads an RNode and/or MQTT observers and stores everything heard.
func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	port := fs.String("port", "", "serial port of a local RNode, e.g. COM32")
	dbPath := fs.String("db", "rradmon.db", "database file")
	freq := fs.Uint("freq", 925875000, "frequency, Hz")
	bw := fs.Uint("bw", 250000, "bandwidth, Hz")
	sf := fs.Uint("sf", 9, "spreading factor")
	cr := fs.Uint("cr", 5, "coding rate denominator (5 = 4/5)")
	promisc := fs.Bool("promisc", true, "raw frames (promiscuous); false = whole packets")
	quiet := fs.Bool("quiet", false, "do not print each packet")
	dur := fs.Duration("for", 0, "stop after this long (0 = until Ctrl+C)")
	httpAddr := fs.String("http", "127.0.0.1:8080", "web UI address (empty to disable)")
	localObs := fs.String("local", "", "treat this MQTT observer id as the local observer (an attached RNode always is)")
	lat := fs.Float64("lat", 0, "position of the local RNode (optional, shown on the map)")
	lon := fs.Float64("lon", 0, "position of the local RNode (optional)")
	broker := brokerFlags(fs)
	allow := fs.String("mqtt-allow", "", "comma-separated observer ids to accept (default all)")
	fs.Parse(args)
	if *port == "" && broker.URL == "" {
		fs.Usage()
		return errors.New("run: give -port, -mqtt, or both")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	radio := ingest.Radio{FreqHz: uint32(*freq), BandwidthHz: uint32(*bw), SF: uint8(*sf), CR: uint8(*cr)}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := setLocalObserver(st, fs, *localObs); err != nil {
		return err
	}
	in, err := ingest.New(st, radio, log)
	if err != nil {
		return err
	}
	st.OnForget = in.Forget // a forgotten observer that is still sending comes back as new
	ws, shutdown, err := startHTTP(*httpAddr, st, log)
	if err != nil {
		return err
	}
	defer shutdown()
	in.OnEvent = func(ev ingest.Event) {
		if !*quiet {
			fmt.Println(describeEvent(ev))
		}
		if ws != nil {
			ws.Notify(ev.T)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}

	if broker.URL != "" {
		var ids []string
		if *allow != "" {
			ids = strings.Split(*allow, ",")
		}
		h := &mqttIngest{in: in, st: st, network: radio.Slug(), log: log}
		sub, err := mqtt.NewSubscriber(*broker, h, mqtt.SubscribeOptions{Allow: ids}, log)
		if err != nil {
			return err
		}
		defer sub.Close()
		if ws != nil {
			ws.Counters = func() map[string]web.Counters {
				out := map[string]web.Counters{}
				for id, c := range sub.Stats() {
					out[id] = web.Counters{Accepted: c.Accepted, Invalid: c.Invalid,
						RateLimited: c.RateLimited, SeqGaps: c.SeqGaps}
				}
				return out
			}
		}
		fmt.Fprintf(os.Stderr, "Subscribed to %s for network %s.\n", broker.Redacted(), radio.Slug())
	}

	// Nil channels never fire, so the loop below also works without a radio.
	var (
		dev     *rnode.Device
		devDone <-chan struct{}
		events  chan rnode.Event
		cfg     rnode.RadioConfig
	)
	if *port != "" {
		dev, err = rnode.Open(*port, log)
		if err != nil {
			return err
		}
		defer dev.Close()
		cfg = rnode.RadioConfig{FreqHz: radio.FreqHz, BandwidthHz: radio.BandwidthHz, SF: radio.SF, CR: radio.CR,
			TxPowerDBm: 2, Promiscuous: *promisc}
		events = make(chan rnode.Event, 256)
		fmt.Fprintf(os.Stderr, "Opening %s, configuring %s ...\n", *port, cfg)
		if err := dev.Start(ctx, cfg, events); err != nil {
			return err
		}
		devDone = dev.Done()
		info := dev.Info()
		fw := fmt.Sprintf("RNode %d.%d %s", info.FirmwareMajor, info.FirmwareMinor, rnode.PlatformName(info.Platform))
		fmt.Fprintf(os.Stderr, "%s.\n", fw)
		local := "rnode:" + *port
		if err := st.ObserverStatus(local, "rnode-serial", time.Now(), store.ObserverState{
			Online: true, Firmware: fw, Software: "rradmon " + version, Lat: optPos(*lat, *lon), Lon: optPos(*lon, *lat)}); err != nil {
			return err
		}
		defer func() {
			st.ObserverStatus(local, "rnode-serial", time.Now(), store.ObserverState{Online: false})
		}()
	}
	fmt.Fprintf(os.Stderr, "Storing in %s. Ctrl+C to stop.\n", *dbPath)

	obs, src := "rnode:"+*port, "rnode-serial"
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	purge := time.NewTicker(time.Hour)
	defer purge.Stop()
	doPurge := func() {
		r, err := st.Purge(ctx, time.Now())
		if err != nil && ctx.Err() == nil {
			log.Error("purge failed", "err", err)
		} else if r != (store.PurgeResult{}) {
			log.Info("purged old data", "receptions", r.Receptions, "packets", r.Packets, "announces", r.Announces,
				"observers", r.Observers)
		}
	}
	doPurge()

	for {
		select {
		case <-ctx.Done():
			in.Flush(time.Now().Add(time.Hour))
			fmt.Fprintln(os.Stderr, "\nStopped.")
			return printSummary(st)
		case <-devDone:
			if err := dev.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
				return fmt.Errorf("device stopped: %w", err)
			}
			return nil
		case now := <-tick.C:
			if err := in.Flush(now); err != nil {
				return err
			}
		case <-purge.C:
			doPurge()
		case ev := <-events:
			switch e := ev.(type) {
			case rnode.Frame:
				f := reassembly.Frame{T: e.T, RSSI: e.RSSI, SNR: e.SNR, HasSignal: e.HasSignal, Data: e.Data}
				if err := in.Frame(obs, src, f, e.Raw); err != nil {
					return err
				}
			case rnode.ChannelStats:
				if err := in.Channel(obs, src, store.ChannelStats{T: e.T, AirtimeShort: e.AirtimeShort,
					AirtimeLong: e.AirtimeLong, UtilShort: e.UtilShort, UtilLong: e.UtilLong,
					RSSI: e.RSSI, NoiseFloor: e.NoiseFloor, Interference: e.Interference}); err != nil {
					return err
				}
				if ws != nil {
					ws.Notify(e.T)
				}
			case rnode.DeviceError:
				log.Warn("device error", "text", e.Text)
				if e.Code == 0xF8 {
					go func() {
						if err := dev.Configure(ctx, cfg); err != nil {
							log.Error("reconfigure after reset failed", "err", err)
							stop()
						}
					}()
				}
			}
		}
	}
}

// runImport stores a capture file, as if it had been heard live.
func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dbPath := fs.String("db", "rradmon.db", "database file")
	quiet := fs.Bool("quiet", false, "do not print each packet")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: rradmon import [-db file] <capture.jsonl>")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := importCapture(st, fs.Arg(0), *quiet)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Imported %d records into %s.\n", n, *dbPath)
	return printSummary(st)
}

func importCapture(st *store.Store, path string, quiet bool) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var (
		in   *ingest.Ingestor
		obs  = "capture"
		src  = "capture:" + path
		raw  = true
		n    int
		last time.Time
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var rec struct {
			decode.Record
			Port         string   `json:"port"`
			FreqHz       uint32   `json:"freq_hz"`
			AirtimeShort float32  `json:"airtime_s"`
			AirtimeLong  float32  `json:"airtime_l"`
			UtilShort    float32  `json:"util_s"`
			UtilLong     float32  `json:"util_l"`
			NoiseFloor   float32  `json:"noise_floor"`
			Interference *float32 `json:"interference"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return n, err
		}
		t, _ := time.Parse(time.RFC3339Nano, rec.T)
		if rec.Type != "session" && in == nil {
			return n, errors.New("import: capture has no session record first")
		}
		switch rec.Type {
		case "session":
			in, err = ingest.New(st, ingest.Radio{FreqHz: rec.FreqHz, BandwidthHz: rec.BwHz, SF: rec.SF, CR: rec.CR}, nil)
			if err != nil {
				return n, err
			}
			if !quiet {
				in.OnEvent = func(ev ingest.Event) { fmt.Println(describeEvent(ev)) }
			}
			if rec.Port != "" {
				obs = "rnode:" + rec.Port
			}
			if rec.Promisc != nil {
				raw = *rec.Promisc
			}
		case "frame":
			data, err := hex.DecodeString(rec.Frame)
			if err != nil {
				return n, err
			}
			fr := reassembly.Frame{T: t, Data: data}
			if rec.RSSI != nil && rec.SNR != nil {
				fr.RSSI, fr.SNR, fr.HasSignal = *rec.RSSI, *rec.SNR, true
			}
			isRaw := raw
			if rec.Raw != nil {
				isRaw = *rec.Raw
			}
			if err := in.Frame(obs, src, fr, isRaw); err != nil {
				return n, err
			}
		case "chan":
			rssi := float32(0)
			if rec.RSSI != nil {
				rssi = *rec.RSSI
			}
			if err := in.Channel(obs, src, store.ChannelStats{T: t, AirtimeShort: rec.AirtimeShort,
				AirtimeLong: rec.AirtimeLong, UtilShort: rec.UtilShort, UtilLong: rec.UtilLong,
				RSSI: rssi, NoiseFloor: rec.NoiseFloor, Interference: rec.Interference}); err != nil {
				return n, err
			}
		}
		if !t.IsZero() {
			last = t
		}
		n++
	}
	if in != nil {
		if err := in.Flush(last.Add(time.Hour)); err != nil {
			return n, err
		}
	}
	return n, sc.Err()
}

func describeEvent(ev ingest.Event) string {
	it := ev.Item
	r := it.R
	sig := "    no signal     "
	if r.HasSignal {
		sig = fmt.Sprintf("%6.1f dBm %5.2f dB", r.RSSI, r.SNR)
	}
	s := fmt.Sprintf("%s %s %3dB", r.T.Local().Format("15:04:05.000"), sig, len(r.Packet))
	if it.Packet == nil || ev.Rec.PacketID == 0 {
		s += " " + r.Part.String()
		if it.Err != nil && r.Part == reassembly.Whole {
			s += fmt.Sprintf(" (%v)", it.Err)
		}
		if ev.Matched != 0 {
			s += fmt.Sprintf(", partial reception of packet #%d", ev.Matched)
		}
		return s
	}
	p := it.Packet
	s += fmt.Sprintf(" #%-5d %-11s hops=%d dst=%s", ev.Rec.PacketID, p.Type, p.Hops, hex.EncodeToString(p.Dest[:6]))
	if r.Frames == 2 {
		s += " [split]"
	}
	if a := it.Announce; a != nil {
		name := reticulum.WellKnownName(a.NameHash)
		if dn := reticulum.DisplayName(a.NameHash, a.AppData); dn != "" {
			name += " \"" + dn + "\""
		}
		s += fmt.Sprintf(" %s %s", name, a.Validity)
	}
	switch {
	case !ev.Rec.New:
		s += fmt.Sprintf(" (again, +%s)", ev.Rec.Delay.Round(time.Millisecond))
	case ev.Rec.RepeatOf != 0:
		s += fmt.Sprintf(" (repeat of #%d)", ev.Rec.RepeatOf)
	}
	if ev.Rec.TxIdentity != nil {
		s += " from " + hex.EncodeToString(ev.Rec.TxIdentity[:4])
	}
	if ev.Rec.Partials > 0 {
		s += fmt.Sprintf(" +%d earlier partial", ev.Rec.Partials)
	}
	return s
}

func printSummary(st *store.Store) error {
	q := func(sql string) int64 {
		var n int64
		st.DB.QueryRow(sql).Scan(&n)
		return n
	}
	fmt.Fprintf(os.Stderr, "Database: %d packets, %d receptions (%d partial), %d destinations, %d announces, %d frame errors, %d channel reports.\n",
		q(`SELECT COUNT(*) FROM packets`), q(`SELECT COUNT(*) FROM receptions`),
		q(`SELECT COUNT(*) FROM receptions WHERE partial > 0`), q(`SELECT COUNT(*) FROM destinations`),
		q(`SELECT COUNT(*) FROM announces`), q(`SELECT COUNT(*) FROM frame_errors`),
		q(`SELECT COUNT(*) FROM channel_stats`))
	return nil
}

// optPos returns &v when a position was given (not both zero).
func optPos(v, other float64) *float64 {
	if v == 0 && other == 0 {
		return nil
	}
	return &v
}
