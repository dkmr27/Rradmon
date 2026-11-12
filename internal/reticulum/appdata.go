package reticulum

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// DisplayName extracts a human-readable name from announce app data, best
// effort. NomadNet nodes announce a plain UTF-8 name; LXMF delivery
// destinations announce a msgpack array whose first element is the name
// (older versions sent the name as plain bytes). Returns "" if none is found.
func DisplayName(nameHash, appData []byte) string {
	if len(appData) == 0 {
		return ""
	}
	if b := appData[0]; b > 0x90 && b <= 0x9f { // non-empty fixarray
		if s, ok := msgpackString(appData[1:]); ok {
			return clean(s)
		}
		return ""
	}
	if utf8.Valid(appData) {
		return clean(string(appData))
	}
	return ""
}

// msgpackString reads a str or bin value at the start of b.
func msgpackString(b []byte) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	var n, at int
	switch t := b[0]; {
	case t >= 0xa0 && t <= 0xbf: // fixstr
		n, at = int(t&0x1f), 1
	case t == 0xd9 || t == 0xc4: // str8, bin8
		if len(b) < 2 {
			return "", false
		}
		n, at = int(b[1]), 2
	case t == 0xda || t == 0xc5: // str16, bin16
		if len(b) < 3 {
			return "", false
		}
		n, at = int(b[1])<<8|int(b[2]), 3
	default:
		return "", false
	}
	if len(b) < at+n || !utf8.Valid(b[at:at+n]) {
		return "", false
	}
	return string(b[at : at+n]), true
}

// clean keeps printable text, trims it and caps its length.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64])
	}
	return s
}
