package mqtt

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// Handler receives validated messages. tRx is when the server received it;
// tObs is the observer's timestamp (zero if missing or implausible).
type Handler interface {
	Rx(network, observer string, m *Rx, tRx, tObs time.Time)
	Chan(network, observer string, m *Chan, tRx, tObs time.Time)
	// raw is the message as received, for fields beyond Status.
	Status(network, observer string, m *Status, raw []byte, tRx time.Time)
}

// Limits on inbound data (spec §5: inbound data is untrusted).
const (
	maxMessageBytes = 4096
	rateBurst       = 100 // messages
	ratePerSecond   = 20  // sustained, per observer
)

// SubStats counts what the subscriber dropped, per observer.
type SubStats struct {
	Accepted, Invalid, RateLimited, SeqGaps uint64
}

// Subscriber receives all observers under the prefix.
type Subscriber struct {
	c      paho.Client
	prefix string
	h      Handler
	log    *slog.Logger
	allow  map[string]bool // nil = any observer

	mu      sync.Mutex
	buckets map[string]*bucket
	lastSeq map[string]uint64
	stats   map[string]*SubStats
}

type bucket struct {
	tokens float64
	last   time.Time
}

// SubscribeOptions narrows what is accepted.
type SubscribeOptions struct {
	// Allow, if not empty, is the list of observer IDs to accept.
	Allow []string
}

// NewSubscriber connects and subscribes to <prefix>/+/+/{rx,chan,status}.
func NewSubscriber(cfg BrokerConfig, h Handler, opt SubscribeOptions, log *slog.Logger) (*Subscriber, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "rradmon-server"
	}
	s := &Subscriber{prefix: cfg.prefix(), h: h, log: log,
		buckets: map[string]*bucket{}, lastSeq: map[string]uint64{}, stats: map[string]*SubStats{}}
	if len(opt.Allow) > 0 {
		s.allow = map[string]bool{}
		for _, id := range opt.Allow {
			s.allow[id] = true
		}
	}
	o, err := cfg.options(log)
	if err != nil {
		return nil, err
	}
	filters := map[string]byte{
		s.prefix + "/+/+/rx":     1,
		s.prefix + "/+/+/chan":   0,
		s.prefix + "/+/+/status": 1,
	}
	// Subscribe in the connect handler so a reconnect resubscribes.
	o.SetOnConnectHandler(func(c paho.Client) {
		log.Info("mqtt connected", "broker", cfg.Redacted(), "prefix", s.prefix)
		if err := wait(c.SubscribeMultiple(filters, s.onMessage), "subscribe"); err != nil {
			log.Error("mqtt subscribe failed", "err", err)
		}
	})
	s.c = paho.NewClient(o)
	if err := wait(s.c.Connect(), "connect to "+cfg.Redacted()); err != nil {
		return nil, err
	}
	return s, nil
}

// Close disconnects.
func (s *Subscriber) Close() { s.c.Disconnect(500) }

// Stats returns a copy of the per-observer counters.
func (s *Subscriber) Stats() map[string]SubStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]SubStats, len(s.stats))
	for k, v := range s.stats {
		out[k] = *v
	}
	return out
}

func (s *Subscriber) stat(obs string) *SubStats {
	st, ok := s.stats[obs]
	if !ok {
		st = &SubStats{}
		s.stats[obs] = st
	}
	return st
}

// allowRate is a token bucket per observer.
func (s *Subscriber) allowRate(obs string, now time.Time) bool {
	b, ok := s.buckets[obs]
	if !ok {
		b = &bucket{tokens: rateBurst, last: now}
		s.buckets[obs] = b
	}
	b.tokens = min(rateBurst, b.tokens+now.Sub(b.last).Seconds()*ratePerSecond)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (s *Subscriber) onMessage(_ paho.Client, msg paho.Message) {
	s.handle(msg.Topic(), msg.Payload(), time.Now())
}

// handle is onMessage without the client, for tests.
func (s *Subscriber) handle(topic string, payload []byte, now time.Time) {
	network, obs, kind, ok := ParseTopic(s.prefix, topic)
	if !ok || !ValidObserverID(obs) {
		return
	}
	if s.allow != nil && !s.allow[obs] {
		return
	}
	s.mu.Lock()
	st := s.stat(obs)
	if len(payload) > maxMessageBytes {
		st.Invalid++
		s.mu.Unlock()
		return
	}
	if kind != "status" && !s.allowRate(obs, now) {
		st.RateLimited++
		n := st.RateLimited
		s.mu.Unlock()
		if n == 1 || n%1000 == 0 {
			s.log.Warn("mqtt observer rate limited", "observer", obs, "dropped", n)
		}
		return
	}
	s.mu.Unlock()

	bad := func(err error) {
		s.mu.Lock()
		st.Invalid++
		n := st.Invalid
		s.mu.Unlock()
		if n <= 5 || n%1000 == 0 {
			s.log.Warn("mqtt message rejected", "observer", obs, "kind", kind, "err", err)
		}
	}

	switch kind {
	case "rx":
		var m Rx
		if err := json.Unmarshal(payload, &m); err != nil {
			bad(err)
			return
		}
		tObs, err := m.Validate(obs, now)
		if err != nil {
			bad(err)
			return
		}
		if network != m.Radio.NetworkID() {
			bad(errNetwork)
			return
		}
		s.mu.Lock()
		if last, ok := s.lastSeq[obs]; ok && m.Seq > last+1 {
			st.SeqGaps += m.Seq - last - 1
		}
		if m.Seq > s.lastSeq[obs] || m.Seq == 1 {
			s.lastSeq[obs] = m.Seq
		}
		st.Accepted++
		s.mu.Unlock()
		s.h.Rx(network, obs, &m, now, tObs)
	case "chan":
		var m Chan
		if err := json.Unmarshal(payload, &m); err != nil {
			bad(err)
			return
		}
		tObs, err := m.Validate(obs, now)
		if err != nil {
			bad(err)
			return
		}
		s.h.Chan(network, obs, &m, now, tObs)
	case "status":
		var m Status
		if err := json.Unmarshal(payload, &m); err != nil || m.V != Version || m.Observer != obs {
			bad(errObs)
			return
		}
		s.h.Status(network, obs, &m, payload, now)
	}
}

var errNetwork = errorString("radio settings do not match the topic's network")

type errorString string

func (e errorString) Error() string { return string(e) }
