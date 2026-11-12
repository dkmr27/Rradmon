package rnode

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dkmr27/Rradmon/internal/kiss"
)

// fakeRNode answers host commands the way RNode firmware does: it echoes
// settings, replies to detect, and reports promiscuous state.
type fakeRNode struct {
	hostToDev *io.PipeReader
	devToHost *io.PipeWriter
	promisc   chan struct{}
	once      sync.Once
}

type pipeConn struct {
	io.Reader
	io.Writer
	closers []io.Closer
}

func (p pipeConn) Close() error {
	for _, c := range p.closers {
		c.Close()
	}
	return nil
}

func newFake(t *testing.T) (*fakeRNode, io.ReadWriteCloser) {
	hr, hw := io.Pipe() // host -> device
	dr, dw := io.Pipe() // device -> host
	f := &fakeRNode{hostToDev: hr, devToHost: dw, promisc: make(chan struct{})}
	go f.run()
	t.Cleanup(func() { hr.Close(); dw.Close() })
	return f, pipeConn{Reader: dr, Writer: hw, closers: []io.Closer{hw, dr}}
}

func (f *fakeRNode) send(cmd byte, data ...byte) {
	f.devToHost.Write(kiss.Encode(cmd, data))
}

func (f *fakeRNode) run() {
	var dec kiss.Decoder
	buf := make([]byte, 256)
	for {
		n, err := f.hostToDev.Read(buf)
		if err != nil {
			return
		}
		for _, fr := range dec.Feed(buf[:n]) {
			switch fr.Cmd {
			case CmdDetect:
				f.send(CmdDetect, detectResp)
			case CmdFwVersion:
				f.send(CmdFwVersion, 1, 82)
			case CmdPlatform:
				f.send(CmdPlatform, 0x80)
			case CmdMCU:
				f.send(CmdMCU, 0x81)
			case CmdFrequency, CmdBandwidth, CmdTxPower, CmdSF, CmdCR, CmdRadioState:
				f.send(fr.Cmd, fr.Data...)
			case CmdPromisc:
				f.send(CmdPromisc, fr.Data...)
				if len(fr.Data) > 0 && fr.Data[0] == 1 {
					f.once.Do(func() { close(f.promisc) })
				}
			}
		}
	}
}

var testCfg = RadioConfig{FreqHz: 925875000, BandwidthHz: 250000, SF: 9, CR: 5, TxPowerDBm: 2, Promiscuous: true}

func TestStartAndReceive(t *testing.T) {
	fake, conn := newFake(t)
	dev := New(conn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make(chan Event, 16)
	if err := dev.Start(ctx, testCfg, events); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info := dev.Info()
	if info.FirmwareMajor != 1 || info.FirmwareMinor != 82 || PlatformName(info.Platform) != "ESP32" {
		t.Errorf("info = %+v", info)
	}
	<-fake.promisc

	// A frame containing FEND, to check escaping on the way through.
	raw := []byte{0x30, 0x08, 0x00, kiss.FEND, 0x01, 0x02}
	fake.send(CmdStatRSSI, 157-97) // -97 dBm
	fake.send(CmdStatSNR, 0xF6)    // -10 * 0.25 = -2.5 dB
	fake.send(CmdData, raw...)
	fake.send(CmdData, 0x00, 0x01) // no signal reports before this one
	fake.send(CmdStatChtm, 0x00, 0x64, 0x00, 0xC8, 0x03, 0xE8, 0x07, 0xD0, 157-90, 157-120, 0xFF)

	got := []Event{<-events, <-events, <-events}

	f1, ok := got[0].(Frame)
	if !ok || !bytes.Equal(f1.Data, raw) || !f1.HasSignal || f1.RSSI != -97 || f1.SNR != -2.5 {
		t.Errorf("frame 1 = %+v", got[0])
	}
	f2, ok := got[1].(Frame)
	if !ok || f2.HasSignal {
		t.Errorf("frame 2 should have no signal: %+v", got[1])
	}
	cs, ok := got[2].(ChannelStats)
	if !ok || cs.AirtimeShort != 1 || cs.AirtimeLong != 2 || cs.UtilShort != 10 || cs.UtilLong != 20 ||
		cs.RSSI != -90 || cs.NoiseFloor != -120 || cs.Interference != nil {
		t.Errorf("channel stats = %+v", got[2])
	}
}

func TestResetHandling(t *testing.T) {
	fake, conn := newFake(t)
	dev := New(conn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events := make(chan Event, 16)

	// A boot reset before setup must not surface as an event. The pipe is
	// synchronous, so write it from a goroutine; Start's reader drains it.
	go fake.send(CmdReset, 0xF8)
	if err := dev.Start(ctx, testCfg, events); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.send(CmdReset, 0xF8)
	ev := <-events
	if de, ok := ev.(DeviceError); !ok || de.Code != 0xF8 {
		t.Fatalf("first event = %+v, want reset after setup", ev)
	}
	if err := dev.Configure(ctx, testCfg); err != nil {
		t.Fatalf("Configure after reset: %v", err)
	}
}

func TestValidateMismatch(t *testing.T) {
	_, conn := newFake(t)
	dev := New(conn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dev.Start(ctx, testCfg, make(chan Event, 16)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	other := testCfg
	other.SF = 10
	if err := dev.validate(other); err == nil {
		t.Fatal("validate accepted a different SF")
	}
}
