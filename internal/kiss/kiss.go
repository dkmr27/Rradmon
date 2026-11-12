// Package kiss implements the KISS framing used by RNode devices.
package kiss

const (
	FEND  = 0xC0
	FESC  = 0xDB
	TFEND = 0xDC
	TFESC = 0xDD
)

// MaxFrame bounds a decoded frame so a stream with a lost FEND cannot grow
// a buffer without limit. RNode frames are at most a few hundred bytes; the
// largest status reads (display buffer) are 1024.
const MaxFrame = 2048

// Frame is one decoded KISS frame: the command byte and its unescaped data.
type Frame struct {
	Cmd  byte
	Data []byte
}

// Encode returns cmd and data as one KISS frame, escaping data.
func Encode(cmd byte, data []byte) []byte {
	out := make([]byte, 0, len(data)+4)
	out = append(out, FEND, cmd)
	for _, b := range data {
		switch b {
		case FEND:
			out = append(out, FESC, TFEND)
		case FESC:
			out = append(out, FESC, TFESC)
		default:
			out = append(out, b)
		}
	}
	return append(out, FEND)
}

// Decoder turns a byte stream into frames. The zero value is ready to use.
type Decoder struct {
	buf     []byte
	inFrame bool
	escape  bool
	// Dropped counts frames discarded for exceeding MaxFrame.
	Dropped int
}

// Feed consumes bytes and returns the frames completed by them.
func (d *Decoder) Feed(p []byte) []Frame {
	var frames []Frame
	for _, b := range p {
		if b == FEND {
			if d.inFrame && len(d.buf) > 0 {
				data := make([]byte, len(d.buf)-1)
				copy(data, d.buf[1:])
				frames = append(frames, Frame{Cmd: d.buf[0], Data: data})
			}
			// A FEND both ends a frame and may start the next one.
			d.buf = d.buf[:0]
			d.inFrame = true
			d.escape = false
			continue
		}
		if !d.inFrame {
			continue
		}
		if d.escape {
			switch b {
			case TFEND:
				b = FEND
			case TFESC:
				b = FESC
			}
			d.escape = false
		} else if b == FESC {
			d.escape = true
			continue
		}
		if len(d.buf) >= MaxFrame {
			d.Dropped++
			d.buf = d.buf[:0]
			d.inFrame = false
			continue
		}
		d.buf = append(d.buf, b)
	}
	return frames
}
