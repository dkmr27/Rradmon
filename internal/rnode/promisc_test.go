package rnode

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/kiss"
)

// chtm builds a CMD_STAT_CHTM frame with the given short-term utilisation.
func chtm(utilShort float32) kiss.Frame {
	u := uint16(utilShort * 100)
	return kiss.Frame{Cmd: CmdStatChtm, Data: []byte{0, 0, 0, 0, byte(u >> 8), byte(u), 0, 0, 60, 60, 0xFF}}
}

// watchDevice is a Device in promiscuous mode, configured at t0, whose
// warnings go to the returned buffer. Its clock is *now.
func watchDevice(promisc bool, t0 time.Time, now *time.Time) (*Device, *bytes.Buffer) {
	var buf bytes.Buffer
	d := New(nil, slog.New(slog.NewTextHandler(&buf, nil)))
	d.now = func() time.Time { return *now }
	d.promisc.Store(promisc)
	d.promiscSince.Store(t0.UnixNano())
	return d, &buf
}

// feed sends n channel reports a second apart with utilisation util.
func feed(d *Device, now *time.Time, n int, util float32) {
	for i := 0; i < n; i++ {
		*now = now.Add(time.Second)
		d.handle(chtm(util))
	}
}

func TestPromiscWarnsWhenBusyWithoutFrames(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)
	now := t0
	d, log := watchDevice(true, t0, &now)

	feed(d, &now, 60, 0) // a quiet minute: no reason to warn
	feed(d, &now, 40, 15)
	if log.Len() != 0 {
		t.Fatalf("warned before %v: %s", promiscWarnAfter, log)
	}
	feed(d, &now, 30, 15) // past two minutes, 70 busy reports
	if n := strings.Count(log.String(), "-promisc=false"); n != 1 {
		t.Fatalf("want one warning, got %d: %s", n, log)
	}
	feed(d, &now, 120, 15)
	if n := strings.Count(log.String(), "-promisc=false"); n != 1 {
		t.Fatalf("warned again: %s", log)
	}
}

func TestPromiscNoWarning(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)

	t.Run("quiet channel", func(t *testing.T) {
		now := t0
		d, log := watchDevice(true, t0, &now)
		feed(d, &now, 600, 0.5) // below the busy level, as on a quiet channel
		if log.Len() != 0 {
			t.Fatal(log)
		}
	})
	t.Run("frames arrive", func(t *testing.T) {
		now := t0
		d, log := watchDevice(true, t0, &now)
		feed(d, &now, 20, 15)
		d.handle(kiss.Frame{Cmd: CmdData, Data: []byte{0x00, 1, 2, 3}})
		feed(d, &now, 600, 15)
		if log.Len() != 0 {
			t.Fatal(log)
		}
	})
	t.Run("not promiscuous", func(t *testing.T) {
		now := t0
		d, log := watchDevice(false, t0, &now)
		feed(d, &now, 600, 15)
		if log.Len() != 0 {
			t.Fatal(log)
		}
	})
}

// After a device reset Configure runs again: a frame from before it does
// not prove the new session works, and the count starts afresh.
func TestPromiscCheckRestartsAfterConfigure(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)
	now := t0
	d, log := watchDevice(true, t0, &now)
	d.handle(kiss.Frame{Cmd: CmdData, Data: []byte{0x00, 1}})
	feed(d, &now, 60, 15)
	d.promiscSince.Store(now.UnixNano()) // what Configure does
	feed(d, &now, 100, 15)
	if log.Len() != 0 {
		t.Fatalf("warned before %v since the reconfigure: %s", promiscWarnAfter, log)
	}
	feed(d, &now, 30, 15)
	if !strings.Contains(log.String(), "-promisc=false") {
		t.Fatalf("no warning after the reconfigure: %s", log)
	}
}
