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
	"github.com/dkmr27/Rradmon/internal/mqtt"
	"github.com/dkmr27/Rradmon/internal/rnode"
)

// brokerFlags adds the MQTT connection flags. The password is read from a
// file or RRADMON_MQTT_PASSWORD, never taken as a flag value.
func brokerFlags(fs *flag.FlagSet) *mqtt.BrokerConfig {
	c := &mqtt.BrokerConfig{}
	fs.StringVar(&c.URL, "mqtt", "", "MQTT broker URL, e.g. tcp://192.168.1.10:1883 (ssl://, ws://, wss:// also work)")
	fs.StringVar(&c.Username, "mqtt-user", "", "MQTT username")
	fs.StringVar(&c.PasswordFile, "mqtt-pass-file", "", "file holding the MQTT password (else $RRADMON_MQTT_PASSWORD)")
	fs.StringVar(&c.Prefix, "mqtt-prefix", mqtt.DefaultPrefix, "MQTT topic prefix")
	return c
}

// listenFlags configure the built-in MQTT broker (rradmon run).
func listenFlags(fs *flag.FlagSet) *mqtt.ListenConfig {
	c := &mqtt.ListenConfig{}
	fs.StringVar(&c.Addr, "mqtt-listen", "", "run a built-in MQTT broker for observers on this address, e.g. 127.0.0.1:1883 "+
		"(this machine only, no password needed); any other address needs -mqtt-listen-user")
	fs.StringVar(&c.Username, "mqtt-listen-user", "", "user observers log in to the built-in broker with")
	fs.StringVar(&c.PasswordFile, "mqtt-listen-pass-file", "", "file holding that password (else $RRADMON_MQTT_LISTEN_PASSWORD)")
	fs.BoolVar(&c.AllowOpen, "mqtt-listen-open", false, "allow a non-loopback -mqtt-listen without a password "+
		"(only where a firewall or a Docker port on the host's loopback limits who can connect)")
	return c
}

// chanIntervalFlag is how often channel reports (noise floor, utilisation)
// are stored or published.
func chanIntervalFlag(fs *flag.FlagSet) *time.Duration {
	return fs.Duration("chan-interval", rnode.ChannelIntervalDefault, "channel report (noise floor, utilisation) interval, 1m to 10m")
}

func checkChanInterval(d time.Duration) error {
	if d < rnode.ChannelIntervalMin || d > rnode.ChannelIntervalMax {
		return fmt.Errorf("-chan-interval %v: must be from 1m to 10m", d)
	}
	return nil
}

func defaultObserverID(port string) string {
	h, _ := os.Hostname()
	id := strings.ToLower("obs-" + h + "-" + strings.TrimPrefix(port, "/dev/"))
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// runObserve reads an RNode (or replays a capture) and publishes to MQTT.
func runObserve(args []string) error {
	fs := flag.NewFlagSet("observe", flag.ExitOnError)
	port := fs.String("port", "", "serial port of the RNode")
	replay := fs.String("replay", "", "publish a capture file instead of reading a radio (testing)")
	speed := fs.Float64("speed", 0, "replay: 1 = real time, 0 = as fast as the broker allows")
	id := fs.String("id", "", "observer id (default obs-<host>-<port>)")
	freq := fs.Uint("freq", 925875000, "frequency, Hz")
	bw := fs.Uint("bw", 250000, "bandwidth, Hz")
	sf := fs.Uint("sf", 9, "spreading factor")
	cr := fs.Uint("cr", 5, "coding rate denominator (5 = 4/5)")
	promisc := fs.Bool("promisc", true, "raw frames (promiscuous); false = whole packets")
	lat := fs.Float64("lat", 0, "observer latitude (optional, published in status)")
	lon := fs.Float64("lon", 0, "observer longitude (optional)")
	dur := fs.Duration("for", 0, "stop after this long (0 = until Ctrl+C)")
	quiet := fs.Bool("quiet", false, "do not print each frame")
	broker := brokerFlags(fs)
	chanEvery := chanIntervalFlag(fs)
	fs.Parse(args)
	if err := checkChanInterval(*chanEvery); err != nil {
		return err
	}
	if (*port == "") == (*replay == "") {
		fs.Usage()
		return errors.New("observe: give exactly one of -port or -replay")
	}
	if broker.URL == "" {
		return errors.New("observe: -mqtt is required")
	}
	if *id == "" {
		src := *port
		if src == "" {
			src = "replay"
		}
		*id = defaultObserverID(src)
	}
	if !mqtt.ValidObserverID(*id) {
		return fmt.Errorf("observe: invalid observer id %q (letters, digits, . _ -; up to 64)", *id)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	radio := mqtt.Radio{FreqHz: uint32(*freq), BwHz: uint32(*bw), SF: uint8(*sf), CR: uint8(*cr), Sync: 0x12}
	status := mqtt.Status{Software: "rradmon " + version}
	if *lat != 0 || *lon != 0 {
		status.Lat, status.Lon = lat, lon
	}

	if *replay != "" {
		pub, err := mqtt.NewPublisher(*broker, *id, radio, status, log)
		if err != nil {
			return err
		}
		defer pub.Close()
		n, err := replayCapture(ctx, pub, *replay, *speed)
		fmt.Fprintf(os.Stderr, "Published %d frames as %s.\n", n, *id)
		return err
	}

	dev, err := rnode.Open(*port, log)
	if err != nil {
		return err
	}
	defer dev.Close()
	cfg := rnode.RadioConfig{FreqHz: radio.FreqHz, BandwidthHz: radio.BwHz, SF: radio.SF, CR: radio.CR,
		TxPowerDBm: 2, Promiscuous: *promisc}
	events := make(chan rnode.Event, 256)
	if err := dev.Start(ctx, cfg, events); err != nil {
		return err
	}
	info := dev.Info()
	status.Firmware = fmt.Sprintf("RNode %d.%d %s", info.FirmwareMajor, info.FirmwareMinor, rnode.PlatformName(info.Platform))
	pub, err := mqtt.NewPublisher(*broker, *id, radio, status, log)
	if err != nil {
		return err
	}
	defer pub.Close()
	fmt.Fprintf(os.Stderr, "Observer %s: %s on %s -> %s (%s/%s/%s/…). Ctrl+C to stop.\n",
		*id, status.Firmware, *port, broker.Redacted(), strings.TrimSuffix(broker.Prefix, "/"), radio.NetworkID(), *id)

	avg := rnode.ChannelAverager{Interval: *chanEvery}
	heartbeat := time.NewTicker(time.Minute)
	defer heartbeat.Stop()
	var frames int
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\nStopped after %d frames.\n", frames)
			return nil
		case <-dev.Done():
			if err := dev.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
				return fmt.Errorf("device stopped: %w", err)
			}
			return nil
		case <-heartbeat.C:
			pub.Heartbeat()
		case ev := <-events:
			switch e := ev.(type) {
			case rnode.Frame:
				frames++
				if err := pub.Rx(e.T, e.RSSI, e.SNR, e.HasSignal, e.Data, e.Raw); err != nil {
					log.Warn("publish failed", "err", err)
				}
				if !*quiet {
					fmt.Println(describeFrame(e))
				}
			case rnode.ChannelStats:
				e, ok := avg.Add(e)
				if !ok {
					continue
				}
				if err := pub.Chan(mqtt.Chan{TObs: e.T.UTC().Format(time.RFC3339Nano),
					AirtimeShort: e.AirtimeShort, AirtimeLong: e.AirtimeLong, UtilShort: e.UtilShort,
					UtilLong: e.UtilLong, RSSI: e.RSSI, NoiseFloor: e.NoiseFloor, Interference: e.Interference}); err != nil {
					log.Warn("publish failed", "err", err)
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

// replayCapture publishes a capture file's frames, keeping their spacing
// when speed > 0.
func replayCapture(ctx context.Context, pub *mqtt.Publisher, path string, speed float64) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	raw := true
	var n int
	var first time.Time
	start := time.Now()
	for sc.Scan() {
		if ctx.Err() != nil {
			return n, nil
		}
		var rec decode.Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return n, err
		}
		switch rec.Type {
		case "session":
			if rec.Promisc != nil {
				raw = *rec.Promisc
			}
		case "frame":
			t, _ := time.Parse(time.RFC3339Nano, rec.T)
			if first.IsZero() {
				first = t
			}
			if speed > 0 {
				due := start.Add(time.Duration(float64(t.Sub(first)) / speed))
				select {
				case <-time.After(time.Until(due)):
				case <-ctx.Done():
					return n, nil
				}
			}
			data, err := hex.DecodeString(rec.Frame)
			if err != nil {
				return n, err
			}
			isRaw := raw
			if rec.Raw != nil {
				isRaw = *rec.Raw
			}
			var rssi, snr float32
			has := rec.RSSI != nil && rec.SNR != nil
			if has {
				rssi, snr = *rec.RSSI, *rec.SNR
			}
			if err := pub.Rx(time.Now(), rssi, snr, has, data, isRaw); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, sc.Err()
}
