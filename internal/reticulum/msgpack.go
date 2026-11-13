package reticulum

import (
	"encoding/binary"
	"errors"
	"math"
)

// A minimal msgpack decoder for announce app data (LXMF, interface
// discovery). Decodes to nil, bool, int64, uint64, float64, string, []byte,
// []any and map[any]any. Depth and sizes are bounded: app data is untrusted.

var errMsgpack = errors.New("reticulum: bad msgpack")

const msgpackMaxDepth = 8

type mpReader struct {
	b   []byte
	at  int
	err error
}

// msgpackDecode decodes one value from the start of b and returns it with the
// number of bytes used.
func msgpackDecode(b []byte) (any, int, error) {
	r := &mpReader{b: b}
	v := r.value(0)
	if r.err != nil {
		return nil, 0, r.err
	}
	return v, r.at, nil
}

func (r *mpReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.at+n > len(r.b) {
		r.err = errMsgpack
		return nil
	}
	s := r.b[r.at : r.at+n]
	r.at += n
	return s
}

func (r *mpReader) uint(n int) uint64 {
	s := r.take(n)
	if s == nil {
		return 0
	}
	switch n {
	case 1:
		return uint64(s[0])
	case 2:
		return uint64(binary.BigEndian.Uint16(s))
	case 4:
		return uint64(binary.BigEndian.Uint32(s))
	default:
		return binary.BigEndian.Uint64(s)
	}
}

func (r *mpReader) value(depth int) any {
	if depth > msgpackMaxDepth {
		r.err = errMsgpack
		return nil
	}
	t := r.take(1)
	if t == nil {
		return nil
	}
	c := t[0]
	switch {
	case c <= 0x7f:
		return int64(c)
	case c >= 0xe0:
		return int64(int8(c))
	case c >= 0x80 && c <= 0x8f:
		return r.mapOf(int(c&0x0f), depth)
	case c >= 0x90 && c <= 0x9f:
		return r.arrayOf(int(c&0x0f), depth)
	case c >= 0xa0 && c <= 0xbf:
		return string(r.take(int(c & 0x1f)))
	}
	switch c {
	case 0xc0:
		return nil
	case 0xc2:
		return false
	case 0xc3:
		return true
	case 0xc4, 0xc5, 0xc6: // bin 8/16/32
		n := r.uint(1 << (c - 0xc4))
		return append([]byte(nil), r.take(int(n))...)
	case 0xd9, 0xda, 0xdb: // str 8/16/32
		n := r.uint(1 << (c - 0xd9))
		return string(r.take(int(n)))
	case 0xca:
		return float64(math.Float32frombits(uint32(r.uint(4))))
	case 0xcb:
		return math.Float64frombits(r.uint(8))
	case 0xcc, 0xcd, 0xce, 0xcf:
		v := r.uint(1 << (c - 0xcc))
		if v <= math.MaxInt64 {
			return int64(v)
		}
		return v
	case 0xd0:
		return int64(int8(r.uint(1)))
	case 0xd1:
		return int64(int16(r.uint(2)))
	case 0xd2:
		return int64(int32(r.uint(4)))
	case 0xd3:
		return int64(r.uint(8))
	case 0xdc, 0xdd:
		return r.arrayOf(int(r.uint(2<<(c-0xdc))), depth)
	case 0xde, 0xdf:
		return r.mapOf(int(r.uint(2<<(c-0xde))), depth)
	}
	r.err = errMsgpack // ext types and reserved codes are not used here
	return nil
}

func (r *mpReader) arrayOf(n int, depth int) []any {
	if n > len(r.b)-r.at { // each element needs at least one byte
		r.err = errMsgpack
		return nil
	}
	out := make([]any, 0, n)
	for i := 0; i < n && r.err == nil; i++ {
		out = append(out, r.value(depth+1))
	}
	return out
}

func (r *mpReader) mapOf(n int, depth int) map[any]any {
	if 2*n > len(r.b)-r.at {
		r.err = errMsgpack
		return nil
	}
	out := make(map[any]any, n)
	for i := 0; i < n && r.err == nil; i++ {
		k := r.value(depth + 1)
		v := r.value(depth + 1)
		switch k.(type) {
		case []byte, []any, map[any]any:
			continue // not usable as a map key; skip
		}
		out[k] = v
	}
	return out
}
