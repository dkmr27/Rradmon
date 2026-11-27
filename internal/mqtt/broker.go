package mqtt

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// ListenConfig configures the MQTT broker built into Rradmon, for observers
// to publish to when there is no broker of their own.
//
// It listens on loopback only unless told otherwise: an observer on the
// same machine (Rrnodemon next to rnsd, `rradmon observe`) needs nothing
// more. Listening on any other address needs a user and password, or an
// explicit AllowOpen for setups where something else limits who can reach
// it (a firewall, a Docker port published on the host's loopback).
type ListenConfig struct {
	Addr         string // host:port, e.g. 127.0.0.1:1883
	Username     string
	PasswordFile string // else $RRADMON_MQTT_LISTEN_PASSWORD
	AllowOpen    bool   // no password on a non-loopback address
}

// Limits for the built-in broker. Observers send small messages and keep
// one connection each.
const (
	brokerMaxClients    = 64
	brokerMaxPacket     = 8192
	brokerSessionExpiry = 3600 // s a disconnected session is kept
)

// Broker is the built-in MQTT broker. Messages observers publish go to the
// Subscriber's checks and handler directly, as if they had come from an
// external broker.
type Broker struct {
	srv  *mochi.Server
	addr string
}

// IsLoopback reports whether a listen address only accepts connections from
// this machine. An empty host or 0.0.0.0 is every interface.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c ListenConfig) password() (string, error) {
	if c.PasswordFile != "" {
		b, err := os.ReadFile(c.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("mqtt: listen password file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv("RRADMON_MQTT_LISTEN_PASSWORD"), nil
}

// StartBroker starts the built-in broker and feeds what observers publish
// under the subscriber's prefix to it.
func StartBroker(cfg ListenConfig, s *Subscriber, log *slog.Logger) (*Broker, error) {
	if log == nil {
		log = slog.Default()
	}
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		return nil, fmt.Errorf("mqtt: listen address %q: want host:port, e.g. 127.0.0.1:1883", cfg.Addr)
	}
	pw, err := cfg.password()
	if err != nil {
		return nil, err
	}
	if (cfg.Username == "") != (pw == "") {
		return nil, errors.New("mqtt: the built-in broker needs both a user and a password, or neither")
	}
	if !IsLoopback(cfg.Addr) && pw == "" && !cfg.AllowOpen {
		return nil, fmt.Errorf("mqtt: %s is reachable from other machines: give -mqtt-listen-user and a "+
			"password file, or -mqtt-listen-open if a firewall already limits who can connect", cfg.Addr)
	}

	caps := mochi.NewDefaultServerCapabilities()
	caps.MaximumClients = brokerMaxClients
	caps.MaximumPacketSize = brokerMaxPacket
	caps.MaximumSessionExpiryInterval = brokerSessionExpiry
	srv := mochi.New(&mochi.Options{
		Capabilities: caps,
		// The library logs every connection at Info; only its problems matter here
		Logger: slog.New(levelFilter{h: log.Handler(), min: slog.LevelWarn}),
	})
	hook := &brokerHook{sub: s, user: []byte(cfg.Username), pass: []byte(pw), log: log}
	if err := srv.AddHook(hook, nil); err != nil {
		return nil, err
	}
	if err := srv.AddListener(listeners.NewTCP(listeners.Config{ID: "rradmon", Address: cfg.Addr})); err != nil {
		return nil, fmt.Errorf("mqtt: listen on %s: %w", cfg.Addr, err)
	}
	if err := srv.Serve(); err != nil {
		return nil, fmt.Errorf("mqtt: listen on %s: %w", cfg.Addr, err)
	}
	mode := "no password, this machine only"
	switch {
	case pw != "":
		mode = "user and password required"
	case !IsLoopback(cfg.Addr):
		mode = "no password (-mqtt-listen-open)"
	}
	log.Info("built-in MQTT broker listening", "addr", cfg.Addr, "access", mode, "prefix", s.prefix)
	return &Broker{srv: srv, addr: cfg.Addr}, nil
}

// Close stops the broker.
func (b *Broker) Close() { b.srv.Close() }

// Clients is how many clients are connected.
func (b *Broker) Clients() int64 { return b.srv.Info.ClientsConnected }

// brokerHook decides who may connect and what they may publish, and passes
// what they publish to the subscriber.
type brokerHook struct {
	mochi.HookBase
	sub        *Subscriber
	user, pass []byte
	log        *slog.Logger
}

func (h *brokerHook) ID() string { return "rradmon" }

func (h *brokerHook) Provides(b byte) bool {
	return bytes.Contains([]byte{mochi.OnConnectAuthenticate, mochi.OnACLCheck, mochi.OnPublished}, []byte{b})
}

func (h *brokerHook) OnConnectAuthenticate(cl *mochi.Client, pk packets.Packet) bool {
	if len(h.pass) == 0 {
		return true // loopback, or -mqtt-listen-open
	}
	ok := subtle.ConstantTimeCompare(pk.Connect.Username, h.user) == 1 &&
		subtle.ConstantTimeCompare(pk.Connect.Password, h.pass) == 1
	if !ok {
		h.log.Warn("built-in MQTT broker: login refused", "from", cl.Net.Remote, "client", cl.ID)
	}
	return ok
}

// OnACLCheck: observers publish their own messages under the prefix
// (<prefix>/<network>/<observer>/rx|chan|status); clients may also read them,
// as from any broker. Nothing else.
func (h *brokerHook) OnACLCheck(cl *mochi.Client, topic string, write bool) bool {
	if write {
		_, obs, kind, ok := ParseTopic(h.sub.prefix, topic)
		return ok && ValidObserverID(obs) && (kind == "rx" || kind == "chan" || kind == "status")
	}
	return topic == h.sub.prefix+"/#" || strings.HasPrefix(topic, h.sub.prefix+"/")
}

func (h *brokerHook) OnPublished(cl *mochi.Client, pk packets.Packet) {
	h.sub.handle(pk.TopicName, pk.Payload, time.Now())
}

// levelFilter passes on log records at or above min.
type levelFilter struct {
	h   slog.Handler
	min slog.Level
}

func (f levelFilter) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= f.min && f.h.Enabled(ctx, l)
}
func (f levelFilter) Handle(ctx context.Context, r slog.Record) error { return f.h.Handle(ctx, r) }
func (f levelFilter) WithAttrs(a []slog.Attr) slog.Handler {
	return levelFilter{h: f.h.WithAttrs(a), min: f.min}
}
func (f levelFilter) WithGroup(n string) slog.Handler {
	return levelFilter{h: f.h.WithGroup(n), min: f.min}
}
