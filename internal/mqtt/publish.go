package mqtt

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// Publisher sends one observer's frames, channel reports and status.
type Publisher struct {
	c       paho.Client
	prefix  string
	obs     string
	radio   Radio
	network string
	log     *slog.Logger
	started time.Time

	mu     sync.Mutex
	seq    uint64
	status Status
}

// NewPublisher connects and publishes a retained "online" status. The broker
// publishes the retained "offline" status as the last will if the observer
// disappears.
func NewPublisher(cfg BrokerConfig, observer string, radio Radio, status Status, log *slog.Logger) (*Publisher, error) {
	if log == nil {
		log = slog.Default()
	}
	if !ValidObserverID(observer) {
		return nil, errObs
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "rradmon-obs-" + observer
	}
	p := &Publisher{prefix: cfg.prefix(), obs: observer, radio: radio, network: radio.NetworkID(), log: log, started: time.Now()}
	status.V, status.Observer, status.Radio = Version, observer, &radio
	p.status = status

	o, err := cfg.options(log)
	if err != nil {
		return nil, err
	}
	will, _ := json.Marshal(Status{V: Version, Observer: observer, Online: false})
	o.SetBinaryWill(p.topic("status"), will, 1, true)
	o.SetOnConnectHandler(func(c paho.Client) {
		log.Info("mqtt connected", "broker", cfg.Redacted(), "observer", observer)
		p.publishStatus(true)
	})
	p.c = paho.NewClient(o)
	if err := wait(p.c.Connect(), "connect to "+cfg.Redacted()); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Publisher) topic(kind string) string { return Topic(p.prefix, p.network, p.obs, kind) }

func (p *Publisher) publishStatus(online bool) {
	p.mu.Lock()
	s := p.status
	p.mu.Unlock()
	s.Online = online
	s.UptimeS = int64(time.Since(p.started).Seconds())
	s.TObs = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(s)
	// Fire and forget from the connect handler; errors surface on reconnect.
	p.c.Publish(p.topic("status"), 1, true, b)
}

// Heartbeat republishes the retained status (uptime). Call every minute or so.
func (p *Publisher) Heartbeat() { p.publishStatus(true) }

// Rx publishes one frame. raw says whether it includes the RNode header byte.
func (p *Publisher) Rx(t time.Time, rssi, snr float32, hasSignal bool, frame []byte, raw bool) error {
	p.mu.Lock()
	p.seq++
	seq := p.seq
	p.mu.Unlock()
	m := Rx{V: Version, Observer: p.obs, Seq: seq, TObs: t.UTC().Format(time.RFC3339Nano),
		Radio: p.radio, Len: len(frame), Frame: frame}
	if hasSignal {
		m.RSSI, m.SNR = &rssi, &snr
	}
	if !raw {
		f := false
		m.Raw = &f
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// QoS 1: at least once. Duplicates are harmless (same hash, same
	// observer, dedupe window); the seq lets the server count gaps.
	return wait(p.c.Publish(p.topic("rx"), 1, false, b), "publish rx")
}

// Chan publishes a channel report.
func (p *Publisher) Chan(c Chan) error {
	c.V, c.Observer = Version, p.obs
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return wait(p.c.Publish(p.topic("chan"), 0, false, b), "publish chan")
}

// Close publishes "offline" and disconnects.
func (p *Publisher) Close() {
	p.mu.Lock()
	s := p.status
	p.mu.Unlock()
	s.Online = false
	s.TObs = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(s)
	p.c.Publish(p.topic("status"), 1, true, b).WaitTimeout(3 * time.Second)
	p.c.Disconnect(500)
}
