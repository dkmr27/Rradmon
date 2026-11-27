package mqtt

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// The broker calls the handler from its own goroutines
type syncRecorder struct {
	mu sync.Mutex
	recorder
}

func (r *syncRecorder) Rx(n, o string, m *Rx, a, b time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorder.Rx(n, o, m, a, b)
}
func (r *syncRecorder) Chan(n, o string, m *Chan, a, b time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorder.Chan(n, o, m, a, b)
}
func (r *syncRecorder) Status(n, o string, m *Status, raw []byte, a time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorder.Status(n, o, m, raw, a)
}
func (r *syncRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rx)
}

func freeLoopback(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return "127.0.0.1:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func startTestBroker(t *testing.T, cfg ListenConfig) (*Broker, *syncRecorder) {
	t.Helper()
	rec := &syncRecorder{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := StartBroker(cfg, NewIntake("", rec, SubscribeOptions{}, log), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b, rec
}

func connect(addr, user, pass string) (paho.Client, error) {
	o := paho.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("test-" + user + strconv.Itoa(int(time.Now().UnixNano()%1e6))).
		SetUsername(user).SetPassword(pass).SetConnectTimeout(3 * time.Second)
	c := paho.NewClient(o)
	tok := c.Connect()
	if !tok.WaitTimeout(5 * time.Second) {
		return nil, errorString("connect timed out")
	}
	return c, tok.Error()
}

func publish(t *testing.T, c paho.Client, topic string, payload []byte) {
	t.Helper()
	tok := c.Publish(topic, 1, false, payload)
	tok.WaitTimeout(3 * time.Second)
}

func waitFor(cond func() bool) bool {
	for i := 0; i < 100; i++ {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func TestBrokerLoopbackNoPassword(t *testing.T) {
	addr := freeLoopback(t)
	b, rec := startTestBroker(t, ListenConfig{Addr: addr})
	c, err := connect(addr, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(100)
	publish(t, c, topic("obs-a", "rx"), rxMsg("obs-a", 1, []byte{0x08, 0x00, 1, 2, 3}))
	if !waitFor(func() bool { return rec.count() == 1 }) {
		t.Fatalf("rx through the broker: %d received, want 1", rec.count())
	}
	if b.Clients() < 1 {
		t.Errorf("clients %d", b.Clients())
	}
	// Topics outside Rradmon's are refused: with QoS 1 on MQTT 3.1.1 the
	// broker drops the connection, and nothing reaches the handler
	publish(t, c, "other/topic", []byte("x"))
	publish(t, c, "rradmon/v1/net/bad id/rx", rxMsg("obs-a", 2, []byte{0x08, 0x00, 1}))
	time.Sleep(200 * time.Millisecond)
	if rec.count() != 1 {
		t.Errorf("handler got %d, want only the 1 valid message", rec.count())
	}
}

// The broker itself refuses what Rradmon would not accept, so other readers
// of the prefix never see it either
func TestBrokerTopicRules(t *testing.T) {
	addr := freeLoopback(t)
	startTestBroker(t, ListenConfig{Addr: addr})
	reader, err := connect(addr, "reader", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Disconnect(100)
	var mu sync.Mutex
	var got []string
	reader.Subscribe(DefaultPrefix+"/#", 1, func(_ paho.Client, m paho.Message) {
		mu.Lock()
		got = append(got, m.Topic())
		mu.Unlock()
	}).WaitTimeout(3 * time.Second)
	if tok := reader.Subscribe("#", 0, nil); tok.WaitTimeout(3*time.Second) && tok.Error() == nil {
		// paho reports a refused subscription in the SUBACK, not as an error;
		// what matters is that nothing outside the prefix is delivered below
		_ = tok
	}

	for _, tp := range []string{DefaultPrefix + "/net/obs-c/extra/rx", DefaultPrefix + "/net/obs c/rx",
		DefaultPrefix + "/net/obs-c/other", "elsewhere/x"} {
		w, err := connect(addr, "writer", "")
		if err != nil {
			t.Fatal(err)
		}
		w.Publish(tp, 0, false, []byte("x")).WaitTimeout(time.Second)
		w.Disconnect(100)
	}
	w, err := connect(addr, "writer", "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Disconnect(100)
	w.Publish(topic("obs-c", "status"), 1, false, []byte(`{"v":1,"observer":"obs-c","online":true}`)).WaitTimeout(time.Second)
	waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return len(got) >= 1 })
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != topic("obs-c", "status") {
		t.Errorf("the reader got %v, want only the valid status", got)
	}
}

func TestBrokerPassword(t *testing.T) {
	addr := freeLoopback(t)
	pwFile := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(pwFile, []byte("s3cret-test\n"), 0o600)
	_, rec := startTestBroker(t, ListenConfig{Addr: addr, Username: "observer", PasswordFile: pwFile})

	for _, bad := range [][2]string{{"", ""}, {"observer", "wrong"}, {"other", "s3cret-test"}} {
		if c, err := connect(addr, bad[0], bad[1]); err == nil {
			c.Disconnect(100)
			t.Errorf("login %q/%q accepted", bad[0], bad[1])
		}
	}
	c, err := connect(addr, "observer", "s3cret-test")
	if err != nil {
		t.Fatalf("right password refused: %v", err)
	}
	defer c.Disconnect(100)
	publish(t, c, topic("obs-b", "rx"), rxMsg("obs-b", 1, []byte{0x08, 0x00, 9}))
	if !waitFor(func() bool { return rec.count() == 1 }) {
		t.Fatalf("rx with a password: %d received", rec.count())
	}
}

func TestBrokerRefusesUnsafeConfigs(t *testing.T) {
	pwFile := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(pwFile, []byte("pw"), 0o600)
	for _, c := range []struct {
		cfg  ListenConfig
		want string
	}{
		{ListenConfig{Addr: "0.0.0.0:1883"}, "reachable from other machines"},
		{ListenConfig{Addr: ":1883"}, "reachable from other machines"},
		{ListenConfig{Addr: "192.168.1.5:1883"}, "reachable from other machines"},
		{ListenConfig{Addr: "127.0.0.1:1883", Username: "u"}, "both a user and a password"},
		{ListenConfig{Addr: "127.0.0.1:1883", PasswordFile: pwFile}, "both a user and a password"},
		{ListenConfig{Addr: "1883"}, "want host:port"},
	} {
		_, err := StartBroker(c.cfg, NewIntake("", &recorder{}, SubscribeOptions{}, nil), nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.cfg, err, c.want)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:1883": true, "localhost:1883": true, "[::1]:1883": true, "127.1.2.3:1883": true,
		"0.0.0.0:1883": false, ":1883": false, "[::]:1883": false, "192.168.1.5:1883": false, "bad": false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v", addr, got)
		}
	}
}
