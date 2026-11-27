package main

import (
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/dkmr27/Rradmon/internal/rnode"
)

// Capture file format: one JSON object per line. The first line is a
// "session" record; then "frame", "chan" and "error" records. Frames are hex
// so the files are readable and diff well; they become the test corpus.

type sessionRec struct {
	Type     string `json:"type"`
	T        string `json:"t"`
	Port     string `json:"port"`
	FreqHz   uint32 `json:"freq_hz"`
	BwHz     uint32 `json:"bw_hz"`
	SF       uint8  `json:"sf"`
	CR       uint8  `json:"cr"`
	Promisc  bool   `json:"promisc"`
	Firmware string `json:"fw"`
	Platform string `json:"platform"`
	Rradmon  string `json:"rradmon"`
}

type frameRec struct {
	Type  string   `json:"type"`
	T     string   `json:"t"`
	RSSI  *float32 `json:"rssi,omitempty"`
	SNR   *float32 `json:"snr,omitempty"`
	Len   int      `json:"len"`
	Raw   bool     `json:"raw"`
	Frame string   `json:"frame"`
}

type chanRec struct {
	Type         string   `json:"type"`
	T            string   `json:"t"`
	AirtimeShort float32  `json:"airtime_s"`
	AirtimeLong  float32  `json:"airtime_l"`
	UtilShort    float32  `json:"util_s"`
	UtilLong     float32  `json:"util_l"`
	RSSI         float32  `json:"rssi"`
	NoiseFloor   float32  `json:"noise_floor"`
	Interference *float32 `json:"interference,omitempty"`
}

type errorRec struct {
	Type string `json:"type"`
	T    string `json:"t"`
	Code byte   `json:"code"`
	Text string `json:"text"`
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func runCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	port := fs.String("port", "", "serial port of the RNode, e.g. COM32 (required)")
	out := fs.String("out", "", "output file (default captures/<time>.jsonl)")
	freq := fs.Uint("freq", 925875000, "frequency, Hz")
	bw := fs.Uint("bw", 250000, "bandwidth, Hz")
	sf := fs.Uint("sf", 9, "spreading factor")
	cr := fs.Uint("cr", 5, "coding rate denominator (5 = 4/5)")
	promisc := fs.Bool("promisc", true, "raw frames (promiscuous); false = whole packets, for firmware whose promiscuous mode does not deliver")
	quiet := fs.Bool("quiet", false, "do not print each frame")
	dur := fs.Duration("for", 0, "stop after this long (0 = until Ctrl+C)")
	fs.Parse(args)
	if *port == "" {
		fs.Usage()
		return errors.New("capture: -port is required")
	}

	cfg := rnode.RadioConfig{FreqHz: uint32(*freq), BandwidthHz: uint32(*bw), SF: uint8(*sf), CR: uint8(*cr), TxPowerDBm: 2, Promiscuous: *promisc}

	if *out == "" {
		*out = filepath.Join("captures", time.Now().Format("20060102-150405")+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	dev, err := rnode.Open(*port, log)
	if err != nil {
		return err
	}
	defer dev.Close()

	events := make(chan rnode.Event, 256)
	fmt.Fprintf(os.Stderr, "Opening %s, configuring %s ...\n", *port, cfg)
	if err := dev.Start(ctx, cfg, events); err != nil {
		return err
	}
	info := dev.Info()
	fw := fmt.Sprintf("%d.%d", info.FirmwareMajor, info.FirmwareMinor)
	mode := "normal mode (whole packets)"
	if cfg.Promiscuous {
		mode = "promiscuous (raw frames)"
	}
	fmt.Fprintf(os.Stderr, "RNode firmware %s on %s, %s. Writing %s. Ctrl+C to stop.\n",
		fw, rnode.PlatformName(info.Platform), mode, *out)

	if err := enc.Encode(sessionRec{
		Type: "session", T: ts(time.Now()), Port: *port,
		FreqHz: cfg.FreqHz, BwHz: cfg.BandwidthHz, SF: cfg.SF, CR: cfg.CR, Promisc: cfg.Promiscuous,
		Firmware: fw, Platform: rnode.PlatformName(info.Platform), Rradmon: version,
	}); err != nil {
		return err
	}

	var frames, chans int
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\nStopped after %s: %d frames, %d channel reports.\n",
				time.Since(start).Round(time.Second), frames, chans)
			return nil
		case <-dev.Done():
			if err := dev.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
				return fmt.Errorf("device stopped: %w", err)
			}
			return nil
		case ev := <-events:
			switch e := ev.(type) {
			case rnode.Frame:
				frames++
				rec := frameRec{Type: "frame", T: ts(e.T), Len: len(e.Data), Raw: e.Raw, Frame: hex.EncodeToString(e.Data)}
				if e.HasSignal {
					rssi, snr := e.RSSI, e.SNR
					rec.RSSI, rec.SNR = &rssi, &snr
				}
				if err := enc.Encode(rec); err != nil {
					return err
				}
				if !*quiet {
					fmt.Println(describeFrame(e))
				}
			case rnode.ChannelStats:
				chans++
				if err := enc.Encode(chanRec{
					Type: "chan", T: ts(e.T), AirtimeShort: e.AirtimeShort, AirtimeLong: e.AirtimeLong,
					UtilShort: e.UtilShort, UtilLong: e.UtilLong, RSSI: e.RSSI, NoiseFloor: e.NoiseFloor,
					Interference: e.Interference,
				}); err != nil {
					return err
				}
			case rnode.DeviceError:
				fmt.Fprintf(os.Stderr, "%s device error: %s\n", e.T.Format("15:04:05"), e.Text)
				if err := enc.Encode(errorRec{Type: "error", T: ts(e.T), Code: e.Code, Text: e.Text}); err != nil {
					return err
				}
				if e.Code == 0xF8 {
					// The radio rebooted and lost its settings. Reconfigure in
					// the background so events keep draining meanwhile.
					go func() {
						if err := dev.Configure(ctx, cfg); err != nil {
							fmt.Fprintln(os.Stderr, "reconfigure after reset failed:", err)
							stop()
						} else {
							fmt.Fprintln(os.Stderr, "reconfigured after reset")
						}
					}()
				}
			}
		}
	}
}

var pktTypes = [4]string{"DATA", "ANNOUNCE", "LINKREQ", "PROOF"}
var dstTypes = [4]string{"single", "group", "plain", "link"}

// describeFrame is a one-line summary for the live view. Full parsing lives
// in the parser package (next step); this only reads the fixed header bytes.
func describeFrame(f rnode.Frame) string {
	sig := "   no signal  "
	if f.HasSignal {
		sig = fmt.Sprintf("%6.1f dBm %5.2f dB", f.RSSI, f.SNR)
	}
	head := fmt.Sprintf("%s %s %3dB", f.T.Format("15:04:05.000"), sig, len(f.Data))
	p := f.Data
	split := ""
	if f.Raw {
		if len(p) < 3 {
			return head + " short frame"
		}
		h := p[0]
		if h&0x01 != 0 {
			split = fmt.Sprintf(" split seq=%x", h>>4)
			if len(p)-1 < 254 {
				return head + split + " (second half)"
			}
		}
		p = p[1:]
	}
	if len(p) < 2 {
		return head + " short packet"
	}
	flags := p[0]
	if flags&0x80 != 0 {
		return head + split + " IFAC"
	}
	s := fmt.Sprintf("%s%s %-8s %-6s hops=%d", head, split, pktTypes[flags&0x03], dstTypes[(flags>>2)&0x03], p[1])
	at := 2
	if flags&0x40 != 0 {
		if len(p) >= 18 {
			s += " via " + hex.EncodeToString(p[2:6])
		}
		at = 18
	}
	if len(p) >= at+16 {
		s += " dst " + hex.EncodeToString(p[at:at+16])
	}
	return s
}

func runPorts() error {
	ports, err := rnode.Ports()
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		fmt.Println("no serial ports found")
	}
	for _, p := range ports {
		fmt.Println(p)
	}
	return nil
}
