// Package rnode drives an RNode as a receive-only promiscuous LoRa sniffer.
//
// Host protocol: KISS frames over serial at 115200 baud. In promiscuous mode
// the RNode sends every received LoRa frame, including the 1-byte RNode
// header, as CMD_STAT_RSSI, CMD_STAT_SNR, then CMD_DATA. See docs/spec-v0.md §2.
package rnode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkmr27/Rradmon/internal/kiss"
)

// KISS commands used by Rradmon.
const (
	CmdData       = 0x00
	CmdFrequency  = 0x01
	CmdBandwidth  = 0x02
	CmdTxPower    = 0x03
	CmdSF         = 0x04
	CmdCR         = 0x05
	CmdRadioState = 0x06
	CmdDetect     = 0x08
	CmdLeave      = 0x0A
	CmdPromisc    = 0x0E
	CmdReady      = 0x0F
	CmdStatRSSI   = 0x23
	CmdStatSNR    = 0x24
	CmdStatChtm   = 0x25
	CmdPlatform   = 0x48
	CmdMCU        = 0x49
	CmdFwVersion  = 0x50
	CmdReset      = 0x55
	CmdError      = 0x90

	detectReq  = 0x73
	detectResp = 0x46

	// RSSIOffset is subtracted from RNode RSSI bytes to give dBm.
	RSSIOffset = 157
)

// RadioConfig is the LoRa modem setting to apply.
type RadioConfig struct {
	FreqHz      uint32
	BandwidthHz uint32
	SF          uint8
	CR          uint8 // 5..8 for 4/5..4/8
	TxPowerDBm  uint8 // unused (Rradmon never transmits) but the firmware requires it
	// Promiscuous asks for raw frames (header byte included, split packets
	// not joined). When false the RNode joins split packets and sends whole
	// Reticulum packets without the header byte; a fallback for firmware
	// whose promiscuous mode does not deliver frames (spec §2).
	Promiscuous bool
}

func (c RadioConfig) String() string {
	return fmt.Sprintf("%.3f MHz, %g kHz, SF%d, CR4/%d", float64(c.FreqHz)/1e6, float64(c.BandwidthHz)/1e3, c.SF, c.CR)
}

// Info is what the device has reported about itself.
type Info struct {
	Detected      bool
	FirmwareMajor byte
	FirmwareMinor byte
	Platform      byte
	MCU           byte
	Reported      RadioConfig
	RadioOn       bool
	Promiscuous   bool
}

// Event is sent on the channel passed to Start.
type Event interface{ isEvent() }

// Frame is one raw LoRa frame heard by the radio.
type Frame struct {
	T         time.Time
	RSSI      float32 // dBm
	SNR       float32 // dB
	HasSignal bool    // RSSI and SNR arrived just before this frame
	Data      []byte  // raw frame incl. RNode header byte if Raw, else a whole Reticulum packet
	Raw       bool
}

// ChannelStats is a periodic CMD_STAT_CHTM report.
type ChannelStats struct {
	T            time.Time
	AirtimeShort float32  // percent
	AirtimeLong  float32  // percent
	UtilShort    float32  // channel utilisation, percent
	UtilLong     float32  // percent
	RSSI         float32  // current channel RSSI, dBm
	NoiseFloor   float32  // dBm
	Interference *float32 // dBm, nil when none detected
}

// DeviceError is a CMD_ERROR report or a device reset.
type DeviceError struct {
	T    time.Time
	Code byte // 0xF8 for a reset
	Text string
}

func (Frame) isEvent()        {}
func (ChannelStats) isEvent() {}
func (DeviceError) isEvent()  {}

// signalMaxAge is how old a RSSI/SNR report may be and still belong to the
// CMD_DATA that follows it. The three frames are written back to back.
const signalMaxAge = 500 * time.Millisecond

// Device is an RNode on a serial port (or any byte stream, for tests).
type Device struct {
	rw  io.ReadWriteCloser
	log *slog.Logger
	now func() time.Time

	writeMu sync.Mutex

	mu       sync.Mutex
	info     Info
	detected chan struct{}

	// Signal for the next CMD_DATA; only touched by the reader goroutine.
	rssi, snr         float32
	rssiAt, snrAt     time.Time
	haveRSSI, haveSNR bool

	configured atomic.Bool
	promisc    atomic.Bool

	done    chan struct{}
	readErr error
}

// New wraps an open byte stream. Call Start to configure and begin reading.
func New(rw io.ReadWriteCloser, log *slog.Logger) *Device {
	if log == nil {
		log = slog.Default()
	}
	return &Device{
		rw:       rw,
		log:      log,
		now:      time.Now,
		detected: make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Info returns a copy of what the device has reported so far.
func (d *Device) Info() Info {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.info
}

// Done is closed when the reader stops; Err then says why.
func (d *Device) Done() <-chan struct{} { return d.done }

// Err returns the reader's error after Done is closed.
func (d *Device) Err() error { return d.readErr }

// Start begins reading, detects the RNode, applies cfg, turns the radio on and
// enables promiscuous mode. Events go to out until the stream ends or ctx is
// cancelled. out should be buffered; the reader blocks when it is full.
//
// ESP32 boards often reboot when the port opens; a reset reported before
// setup finishes is ignored. A reset after that is sent as a DeviceError
// (Code 0xF8), and the caller should call Configure again.
func (d *Device) Start(ctx context.Context, cfg RadioConfig, out chan<- Event) error {
	go d.readLoop(ctx, out)

	if err := d.detect(ctx); err != nil {
		return err
	}
	if err := d.Configure(ctx, cfg); err != nil {
		return err
	}
	d.configured.Store(true)
	return nil
}

// Configure applies cfg, turns the radio on and enables promiscuous mode,
// then checks the device's echoes. Used by Start and after a device reset.
func (d *Device) Configure(ctx context.Context, cfg RadioConfig) error {
	d.mu.Lock()
	d.info.RadioOn, d.info.Promiscuous = false, false
	d.mu.Unlock()
	d.promisc.Store(cfg.Promiscuous)

	cmds := []kissCmd{
		{CmdFrequency, be32(cfg.FreqHz)},
		{CmdBandwidth, be32(cfg.BandwidthHz)},
		{CmdTxPower, []byte{cfg.TxPowerDBm}},
		{CmdSF, []byte{cfg.SF}},
		{CmdCR, []byte{cfg.CR}},
		{CmdRadioState, []byte{0x01}},
		{CmdPromisc, []byte{boolByte(cfg.Promiscuous)}},
	}
	for _, c := range cmds {
		if err := d.send(c.cmd, c.data); err != nil {
			return err
		}
	}

	// The RNode echoes each setting; give it a moment, then check them.
	select {
	case <-time.After(750 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	case <-d.done:
		return fmt.Errorf("rnode: stream closed during setup: %w", d.readErr)
	}
	return d.validate(cfg)
}

// Close turns promiscuous mode off, tells the RNode the host has left and
// closes the stream.
func (d *Device) Close() error {
	_ = d.send(CmdPromisc, []byte{0x00})
	_ = d.send(CmdLeave, []byte{0xFF})
	return d.rw.Close()
}

type kissCmd struct {
	cmd  byte
	data []byte
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func be32(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

func u16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

func (d *Device) send(cmd byte, data []byte) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.rw.Write(kiss.Encode(cmd, data))
	if err != nil {
		return fmt.Errorf("rnode: write cmd 0x%02x: %w", cmd, err)
	}
	return nil
}

// detect asks the device to identify itself, retrying because ESP32 boards
// may reboot when the port opens and miss the first request.
func (d *Device) detect(ctx context.Context) error {
	query := func() error {
		d.writeMu.Lock()
		defer d.writeMu.Unlock()
		var b []byte
		b = append(b, kiss.Encode(CmdDetect, []byte{detectReq})...)
		b = append(b, kiss.Encode(CmdFwVersion, []byte{0x00})...)
		b = append(b, kiss.Encode(CmdPlatform, []byte{0x00})...)
		b = append(b, kiss.Encode(CmdMCU, []byte{0x00})...)
		_, err := d.rw.Write(b)
		return err
	}
	deadline := time.After(8 * time.Second)
	for {
		if err := query(); err != nil {
			return fmt.Errorf("rnode: detect: %w", err)
		}
		select {
		case <-d.detected:
			// Firmware version etc. follow the detect reply closely.
			time.Sleep(200 * time.Millisecond)
			return nil
		case <-time.After(2 * time.Second):
		case <-deadline:
			return errors.New("rnode: no RNode detected (no reply to detect request)")
		case <-ctx.Done():
			return ctx.Err()
		case <-d.done:
			return fmt.Errorf("rnode: stream closed during detect: %w", d.readErr)
		}
	}
}

func (d *Device) validate(want RadioConfig) error {
	info := d.Info()
	got := info.Reported
	var problems []string
	if diff := int64(got.FreqHz) - int64(want.FreqHz); diff > 100 || diff < -100 {
		problems = append(problems, fmt.Sprintf("frequency %d Hz, want %d", got.FreqHz, want.FreqHz))
	}
	if got.BandwidthHz != want.BandwidthHz {
		problems = append(problems, fmt.Sprintf("bandwidth %d Hz, want %d", got.BandwidthHz, want.BandwidthHz))
	}
	if got.SF != want.SF {
		problems = append(problems, fmt.Sprintf("SF %d, want %d", got.SF, want.SF))
	}
	if got.CR != want.CR {
		problems = append(problems, fmt.Sprintf("CR %d, want %d", got.CR, want.CR))
	}
	if !info.RadioOn {
		problems = append(problems, "radio did not report on")
	}
	if info.Promiscuous != want.Promiscuous {
		problems = append(problems, fmt.Sprintf("promiscuous mode %v, want %v", info.Promiscuous, want.Promiscuous))
	}
	if len(problems) > 0 {
		return fmt.Errorf("rnode: configuration not confirmed: %v", problems)
	}
	return nil
}

func (d *Device) readLoop(ctx context.Context, out chan<- Event) {
	defer close(d.done)
	var dec kiss.Decoder
	buf := make([]byte, 1024)
	for {
		n, err := d.rw.Read(buf)
		if n > 0 {
			for _, f := range dec.Feed(buf[:n]) {
				if ev := d.handle(f); ev != nil {
					select {
					case out <- ev:
					case <-ctx.Done():
						d.readErr = ctx.Err()
						return
					}
				}
			}
		}
		if err != nil {
			d.readErr = err
			return
		}
		if ctx.Err() != nil {
			d.readErr = ctx.Err()
			return
		}
	}
}

// handle applies one frame to the device state and returns an event, if any.
func (d *Device) handle(f kiss.Frame) Event {
	now := d.now()
	switch f.Cmd {
	case CmdStatRSSI:
		if len(f.Data) >= 1 {
			d.rssi, d.rssiAt, d.haveRSSI = float32(int(f.Data[0])-RSSIOffset), now, true
		}
	case CmdStatSNR:
		if len(f.Data) >= 1 {
			d.snr, d.snrAt, d.haveSNR = float32(int8(f.Data[0]))*0.25, now, true
		}
	case CmdData:
		fr := Frame{T: now, Data: f.Data, Raw: d.promisc.Load()}
		if d.haveRSSI && d.haveSNR && now.Sub(d.rssiAt) < signalMaxAge && now.Sub(d.snrAt) < signalMaxAge {
			fr.RSSI, fr.SNR, fr.HasSignal = d.rssi, d.snr, true
		}
		d.haveRSSI, d.haveSNR = false, false
		return fr
	case CmdStatChtm:
		if len(f.Data) >= 11 {
			b := f.Data
			cs := ChannelStats{
				T:            now,
				AirtimeShort: float32(u16(b[0:])) / 100,
				AirtimeLong:  float32(u16(b[2:])) / 100,
				UtilShort:    float32(u16(b[4:])) / 100,
				UtilLong:     float32(u16(b[6:])) / 100,
				RSSI:         float32(int(b[8]) - RSSIOffset),
				NoiseFloor:   float32(int(b[9]) - RSSIOffset),
			}
			if b[10] != 0xFF {
				v := float32(int(b[10]) - RSSIOffset)
				cs.Interference = &v
			}
			return cs
		}
	case CmdError:
		code := byte(0)
		if len(f.Data) > 0 {
			code = f.Data[0]
		}
		return DeviceError{T: now, Code: code, Text: errorText(code)}
	case CmdReset:
		if len(f.Data) > 0 && f.Data[0] == 0xF8 {
			if !d.configured.Load() {
				d.log.Debug("rnode: reset during startup ignored")
				return nil
			}
			return DeviceError{T: now, Code: 0xF8, Text: "device reset"}
		}
	default:
		d.updateInfo(f)
	}
	return nil
}

func (d *Device) updateInfo(f kiss.Frame) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := f.Data
	switch f.Cmd {
	case CmdDetect:
		if len(b) >= 1 && b[0] == detectResp && !d.info.Detected {
			d.info.Detected = true
			close(d.detected)
		}
	case CmdFwVersion:
		if len(b) >= 2 {
			d.info.FirmwareMajor, d.info.FirmwareMinor = b[0], b[1]
		}
	case CmdPlatform:
		if len(b) >= 1 {
			d.info.Platform = b[0]
		}
	case CmdMCU:
		if len(b) >= 1 {
			d.info.MCU = b[0]
		}
	case CmdFrequency:
		if len(b) >= 4 {
			d.info.Reported.FreqHz = uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		}
	case CmdBandwidth:
		if len(b) >= 4 {
			d.info.Reported.BandwidthHz = uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		}
	case CmdTxPower:
		if len(b) >= 1 {
			d.info.Reported.TxPowerDBm = b[0]
		}
	case CmdSF:
		if len(b) >= 1 {
			d.info.Reported.SF = b[0]
		}
	case CmdCR:
		if len(b) >= 1 {
			d.info.Reported.CR = b[0]
		}
	case CmdRadioState:
		if len(b) >= 1 {
			d.info.RadioOn = b[0] == 0x01
		}
	case CmdPromisc:
		if len(b) >= 1 {
			d.info.Promiscuous = b[0] == 0x01
		}
	}
}

// PlatformName names the CMD_PLATFORM byte.
func PlatformName(p byte) string {
	switch p {
	case 0x90:
		return "AVR"
	case 0x80:
		return "ESP32"
	case 0x70:
		return "nRF52"
	}
	return fmt.Sprintf("0x%02x", p)
}

func errorText(code byte) string {
	switch code {
	case 0x01:
		return "radio initialisation failed"
	case 0x02:
		return "transmit failed"
	case 0x03:
		return "EEPROM locked"
	case 0x04:
		return "queue full"
	case 0x05:
		return "memory low"
	case 0x06:
		return "modem timeout"
	}
	return fmt.Sprintf("error 0x%02x", code)
}
