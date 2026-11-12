package reticulum

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"time"
)

const (
	pubKeyLen     = 64 // X25519 (32) || Ed25519 (32)
	nameHashLen   = 10
	randomHashLen = 10
	ratchetLen    = 32
	sigLen        = 64
)

// Validity is the result of checking an announce.
type Validity uint8

const (
	Unchecked    Validity = 0
	Valid        Validity = 1
	BadSignature Validity = 2
	HashMismatch Validity = 3 // signature fine, but dest hash is not from this identity
)

func (v Validity) String() string {
	return [...]string{"unchecked", "valid", "bad signature", "hash mismatch"}[v&3]
}

// Announce is the unencrypted body of an ANNOUNCE packet. Slices alias the packet.
type Announce struct {
	Dest         []byte
	PublicKey    []byte
	NameHash     []byte
	RandomHash   []byte
	Ratchet      []byte // nil unless the context flag is set
	Signature    []byte
	AppData      []byte
	IdentityHash []byte
	// Emitted is when the announcer says it made the announce: Reticulum puts
	// a 5-byte Unix time in the second half of the random hash. Zero if the
	// value is implausible. Depends on the announcer's clock.
	Emitted  time.Time
	Validity Validity
}

var ErrNotAnnounce = errors.New("reticulum: not an announce")

// ParseAnnounce extracts and validates the announce in p.
func ParseAnnounce(p *Packet) (*Announce, error) {
	if p.Type != TypeAnnounce {
		return nil, ErrNotAnnounce
	}
	d := p.Data
	need := pubKeyLen + nameHashLen + randomHashLen + sigLen
	if p.ContextFlag {
		need += ratchetLen
	}
	if len(d) < need {
		return nil, ErrShort
	}
	a := &Announce{Dest: p.Dest}
	at := 0
	take := func(n int) []byte { s := d[at : at+n]; at += n; return s }
	a.PublicKey = take(pubKeyLen)
	a.NameHash = take(nameHashLen)
	a.RandomHash = take(randomHashLen)
	if p.ContextFlag {
		a.Ratchet = take(ratchetLen)
	}
	a.Signature = take(sigLen)
	a.AppData = d[at:]

	ih := sha256.Sum256(a.PublicKey)
	a.IdentityHash = ih[:HashLen]

	var ts int64
	for _, b := range a.RandomHash[5:] {
		ts = ts<<8 | int64(b)
	}
	if ts > 1_500_000_000 && ts < 4_000_000_000 {
		a.Emitted = time.Unix(ts, 0)
	}

	a.Validity = a.validate()
	return a, nil
}

func (a *Announce) validate() Validity {
	signed := make([]byte, 0, HashLen+pubKeyLen+nameHashLen+randomHashLen+len(a.Ratchet)+len(a.AppData))
	signed = append(signed, a.Dest...)
	signed = append(signed, a.PublicKey...)
	signed = append(signed, a.NameHash...)
	signed = append(signed, a.RandomHash...)
	signed = append(signed, a.Ratchet...)
	signed = append(signed, a.AppData...)
	if !ed25519.Verify(ed25519.PublicKey(a.PublicKey[32:]), signed, a.Signature) {
		return BadSignature
	}
	material := append(append([]byte{}, a.NameHash...), a.IdentityHash...)
	expected := sha256.Sum256(material)
	if !bytes.Equal(expected[:HashLen], a.Dest) {
		return HashMismatch
	}
	return Valid
}
