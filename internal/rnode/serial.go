package rnode

import (
	"fmt"
	"log/slog"
	"time"

	"go.bug.st/serial"
)

// Open opens an RNode on a serial port (e.g. "COM32" or "/dev/ttyACM0").
func Open(port string, log *slog.Logger) (*Device, error) {
	p, err := serial.Open(port, &serial.Mode{BaudRate: 115200, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit})
	if err != nil {
		return nil, fmt.Errorf("rnode: open %s: %w", port, err)
	}
	// A short read timeout lets the reader notice cancellation.
	if err := p.SetReadTimeout(250 * time.Millisecond); err != nil {
		p.Close()
		return nil, fmt.Errorf("rnode: %s: %w", port, err)
	}
	return New(p, log), nil
}

// Ports lists serial port names on this machine.
func Ports() ([]string, error) { return serial.GetPortsList() }
