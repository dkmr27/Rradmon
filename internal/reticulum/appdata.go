package reticulum

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// DisplayName extracts a human-readable name from announce app data, best
// effort. NomadNet nodes announce a plain UTF-8 name; LXMF delivery
// destinations announce a msgpack array whose first element is the name
// (older versions sent the name as plain bytes); rrcd hubs a CBOR map with
// the hub's name. Returns "" if none is found.
func DisplayName(nameHash, appData []byte) string {
	if len(appData) == 0 {
		return ""
	}
	// rrcd hubs announce a CBOR map with the hub's name
	if WellKnownName(nameHash) == "rrc.hub" {
		if h, ok := ParseRRCHub(appData); ok {
			return clean(h.Name)
		}
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
// MaxTextRunes caps any text taken from app data. Announces are untrusted
// broadcasts: a name must not be able to flood or distort the page.
const MaxTextRunes = 64

// clean makes untrusted text safe to show on one line:
//   - drops control and format characters (newlines, tabs, zero-width and
//     bidirectional overrides such as U+202E, which could reverse text);
//   - collapses runs of whitespace to one space;
//   - keeps at most 2 combining marks per character, so "stacked" text
//     cannot spill into the rows above and below;
//   - caps the result at MaxTextRunes, marking a cut with an ellipsis.
func clean(s string) string {
	var b strings.Builder
	marks, space := 0, false
	n := 0
	cut := false
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
			marks++
			if marks > 2 || n == 0 {
				continue
			}
		case unicode.IsSpace(r):
			if !space && n > 0 {
				space = true
			}
			continue
		case !unicode.IsPrint(r):
			continue
		default:
			marks = 0
			if space {
				b.WriteRune(' ')
				n++
				space = false
			}
		}
		if n >= MaxTextRunes {
			cut = true
			break
		}
		b.WriteRune(r)
		if !unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Me, r) {
			n++
		}
	}
	out := b.String()
	if cut {
		out = strings.TrimSpace(out) + "…"
	}
	return out
}
