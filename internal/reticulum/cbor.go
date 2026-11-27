package reticulum

import (
	"encoding/binary"
	"errors"
	"math"
)

// A minimal CBOR (RFC 8949) decoder for announce app data (rrcd hubs).
// Decodes to nil, bool, int64, uint64, float64, string, []byte, []any and
// map[any]any; tags are skipped and their content kept. Indefinite lengths
// are not supported. Depth and sizes are bounded: app data is untrusted.

var errCBOR = errors.New("reticulum: bad cbor")

const cborMaxDepth = 8

type cborReader struct {
	b   []byte
	at  int
	err error
}

// cborDecode decodes one item from the start of b and returns it with the
// number of bytes used.
func cborDecode(b []byte) (any, int, error) {
	r := &cborReader{b: b}
	v := r.item(0)
	if r.err != nil {
		return nil, 0, r.err
	}
	return v, r.at, nil
}

func (r *cborReader) take(n uint64) []byte {
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.b)-r.at) {
		r.err = errCBOR
		return nil
	}
	s := r.b[r.at : r.at+int(n)]
	r.at += int(n)
	return s
}

// head reads an item's major type and argument
func (r *cborReader) head() (major byte, info byte, arg uint64) {
	h := r.take(1)
	if h == nil {
		return 0, 0, 0
	}
	major, info = h[0]>>5, h[0]&0x1f
	switch {
	case info < 24:
		arg = uint64(info)
	case info == 24:
		if b := r.take(1); b != nil {
			arg = uint64(b[0])
		}
	case info == 25:
		if b := r.take(2); b != nil {
			arg = uint64(binary.BigEndian.Uint16(b))
		}
	case info == 26:
		if b := r.take(4); b != nil {
			arg = uint64(binary.BigEndian.Uint32(b))
		}
	case info == 27:
		if b := r.take(8); b != nil {
			arg = binary.BigEndian.Uint64(b)
		}
	default: // 28-30 reserved, 31 indefinite length
		r.err = errCBOR
	}
	return major, info, arg
}

func (r *cborReader) item(depth int) any {
	if depth > cborMaxDepth {
		r.err = errCBOR
		return nil
	}
	major, info, arg := r.head()
	if r.err != nil {
		return nil
	}
	switch major {
	case 0:
		return arg
	case 1:
		if arg > math.MaxInt64 {
			r.err = errCBOR
			return nil
		}
		return -1 - int64(arg)
	case 2:
		return append([]byte(nil), r.take(arg)...)
	case 3:
		return string(r.take(arg))
	case 4:
		// every item takes at least one byte
		if arg > uint64(len(r.b)-r.at) {
			r.err = errCBOR
			return nil
		}
		a := make([]any, 0, arg)
		for i := uint64(0); i < arg && r.err == nil; i++ {
			a = append(a, r.item(depth+1))
		}
		return a
	case 5:
		if arg > uint64(len(r.b)-r.at)/2 {
			r.err = errCBOR
			return nil
		}
		m := make(map[any]any, arg)
		for i := uint64(0); i < arg && r.err == nil; i++ {
			k := r.item(depth + 1)
			v := r.item(depth + 1)
			switch k.(type) {
			case []any, map[any]any, []byte:
				r.err = errCBOR // not usable as a Go map key
			default:
				m[k] = v
			}
		}
		return m
	case 6: // tag: keep the content
		return r.item(depth + 1)
	default: // 7: simple values and floats
		switch {
		case info == 20:
			return false
		case info == 21:
			return true
		case info == 22 || info == 23:
			return nil
		case info == 25:
			return float64(halfFloat(uint16(arg)))
		case info == 26:
			return float64(math.Float32frombits(uint32(arg)))
		case info == 27:
			return math.Float64frombits(arg)
		}
		return nil
	}
}

// halfFloat converts an IEEE 754 half-precision value
func halfFloat(h uint16) float64 {
	exp := int(h>>10) & 0x1f
	mant := float64(h & 0x3ff)
	var v float64
	switch exp {
	case 0:
		v = math.Ldexp(mant, -24)
	case 31:
		if mant == 0 {
			v = math.Inf(1)
		} else {
			v = math.NaN()
		}
	default:
		v = math.Ldexp(mant+1024, exp-25)
	}
	if h&0x8000 != 0 {
		v = -v
	}
	return v
}
