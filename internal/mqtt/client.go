package mqtt

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// BrokerConfig says how to reach a broker. URLs: tcp://host:1883,
// ssl://host:8883, ws://host/mqtt, wss://host/mqtt. The password comes from
// PasswordFile or the RRADMON_MQTT_PASSWORD environment variable, never a
// flag, so it stays out of shell history and process listings.
type BrokerConfig struct {
	URL          string
	Username     string
	PasswordFile string
	ClientID     string
	Prefix       string // topic prefix, default rradmon/v1
	InsecureTLS  bool   // only for testing; TLS is verified by default
}

func (c BrokerConfig) prefix() string {
	if c.Prefix == "" {
		return DefaultPrefix
	}
	return strings.TrimSuffix(c.Prefix, "/")
}

func (c BrokerConfig) password() (string, error) {
	if c.PasswordFile != "" {
		b, err := os.ReadFile(c.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("mqtt: password file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv("RRADMON_MQTT_PASSWORD"), nil
}

// Redacted is the URL for logs (no credentials can appear in it, but keep
// one place that decides what is printed).
func (c BrokerConfig) Redacted() string {
	if i := strings.Index(c.URL, "@"); i >= 0 {
		if j := strings.Index(c.URL, "://"); j >= 0 {
			return c.URL[:j+3] + "…" + c.URL[i:]
		}
	}
	return c.URL
}

func (c BrokerConfig) options(log *slog.Logger) (*paho.ClientOptions, error) {
	if c.URL == "" {
		return nil, errors.New("mqtt: broker URL is required")
	}
	if strings.Contains(c.URL, "@") {
		return nil, errors.New("mqtt: put credentials in -mqtt-user and a password file, not the URL")
	}
	pw, err := c.password()
	if err != nil {
		return nil, err
	}
	o := paho.NewClientOptions().
		AddBroker(c.URL).
		SetClientID(c.ClientID).
		SetUsername(c.Username).
		SetPassword(pw).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetMaxReconnectInterval(time.Minute).
		SetKeepAlive(30 * time.Second).
		SetOrderMatters(true)
	if strings.HasPrefix(c.URL, "ssl://") || strings.HasPrefix(c.URL, "tls://") ||
		strings.HasPrefix(c.URL, "mqtts://") || strings.HasPrefix(c.URL, "wss://") {
		o.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureTLS})
	}
	o.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Warn("mqtt connection lost, reconnecting", "broker", c.Redacted(), "err", err)
	})
	return o, nil
}

func wait(t paho.Token, what string) error {
	if !t.WaitTimeout(15 * time.Second) {
		return fmt.Errorf("mqtt: %s timed out", what)
	}
	if err := t.Error(); err != nil {
		return fmt.Errorf("mqtt: %s: %w", what, err)
	}
	return nil
}
