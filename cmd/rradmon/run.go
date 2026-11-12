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
	"time"

	"github.com/dkmr27/Rradmon/internal/decode"
	"github.com/dkmr27/Rradmon/internal/ingest"
	"github.com/dkmr27/Rradmon/internal/reassembly"
	"github.com/dkmr27/Rradmon/internal/reticulum"
	"github.com/dkmr27/Rradmon/internal/rnode"
	"github.com/dkmr27/Rradmon/internal/store"
)

// runRun reads an RNode and stores everything it hears.
func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	port := fs.String("port", "", "serial port of the RNode, e.g. COM32 (required)")
	dbPath := fs.String("db", "rradmon.db", "database file")
	freq := fs.Uint("freq", 925875000, "frequency, Hz")
	bw := fs.Uint("bw", 250000, "bandwidth, Hz")
	sf := fs.Uint("sf", 9, "spreading factor")
	cr := fs.Uint("cr", 5, "coding rate denominator (5 = 4/5)")
	promisc := fs.Bool("promisc", true, "raw frames (promiscuous); false = whole packets")
	quiet := fs.Bool("quiet", false, "do not print each packet")
	dur := fs.Duration("for", 0, "stop after this long (0 = until Ctrl+C)")
	fs.Parse(args)
	if *port == "" {
		fs.Usage()
		return errors.New("run: -port is required")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	radio := ingest.Radio{FreqHz: uint32(*freq), BandwidthHz: uint32(*bw), SF: uint8(*sf), CR: uint8(*cr)}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	in, err := ingest.New(st, radio, log)
	if err != nil {
		return err
	}
	if !*quiet {
		in.OnEvent = func(ev ingest.Event) { fmt.Println(describeEvent(ev)) }
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}

	dev, err := rnode.Open(*port, log)
	if err != nil {
		return err
	}
	defer dev.Close()
	cfg := rnode.RadioConfig{FreqHz: radio.FreqHz, BandwidthHz: radio.BandwidthHz, SF: radio.SF, CR: radio.CR,
		TxPowerDBm: 2, Promiscuous: *promisc}
	events := make(chan rnode.Event, 256)
	fmt.Fprintf(os.Stderr, "Opening %s, configuring %s ...\n", *port, cfg)
	if err := dev.Start(ctx, cfg, events); err != nil {
		return err
	}
	info := dev.Info()
	fmt.Fprintf(os.Stderr, "RNode firmware %d.%d on %s. Storing in %s. Ctrl+C to stop.\n",
		info.FirmwareMajor, info.FirmwareMinor, rnode.PlatformName(info.Platform), *dbPath)

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
			log.Info("purged old data", "receptions", r.Receptions, "packets", r.Packets, "announces", r.Announces)
		}
	}
	doPurge()

	for {
		select {
		case <-ctx.Done():
			in.Flush(time.Now().Add(time.Hour))
			fmt.Fprintln(os.Stderr, "\nStopped.")
			return printSummary(st)
		case <-dev.Done():
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
