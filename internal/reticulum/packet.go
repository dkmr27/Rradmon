// Package reticulum parses Reticulum packet headers and announces as seen
// on the air. It reads only what is unencrypted. See docs/spec-v0.md §3–4.
package reticulum

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

// PacketType is the low two bits of the flags byte.
type PacketType uint8

const (
	TypeData        PacketType = 0
	TypeAnnounce    PacketType = 1
	TypeLinkRequest PacketType = 2
	TypeProof       PacketType = 3
)

func (t PacketType) String() string {
	return [...]string{"DATA", "ANNOUNCE", "LINKREQUEST", "PROOF"}[t&3]
}

// DestType is bits 2–3 of the flags byte.
type DestType uint8

const (
	DestSingle DestType = 0
	DestGroup  DestType = 1
	DestPlain  DestType = 2
	DestLink   DestType = 3
)

func (t DestType) String() string {
	return [...]string{"single", "group", "plain", "link"}[t&3]
}

const (
	HashLen          = 16  // truncated destination / identity hash
	MTU              = 508 // largest Reticulum packet on LoRa
	headerMinLen     = 2 + HashLen + 1
	flagIFAC         = 0x80
	flagHeader2      = 0x40
	flagContext      = 0x20
	flagTransport    = 0x10
	maskDestType     = 0x0C
	maskPacketType   = 0x03
	maskHashableFlag = 0x0F
)

var (
	ErrShort = errors.New("reticulum: packet too short")
	ErrLong  = errors.New("reticulum: packet longer than MTU")
	// ErrIFAC means the packet is IFAC-masked: header fields are not readable.
	ErrIFAC = errors.New("reticulum: IFAC-protected packet")
)

// Packet is a parsed Reticulum packet. Slices alias Raw.
type Packet struct {
	Raw         []byte
	Flags       byte
	Hops        uint8
	Header2     bool // transport header: carries the relaying node's transport ID
	ContextFlag bool
	Transport   bool // propagation type: transport (true) or broadcast
	DestType    DestType
	Type        PacketType
	TransportID []byte // Header2 only
	Dest        []byte
	Context     byte
	Data        []byte
}

// Parse reads the header of a whole Reticulum packet (no RNode header byte).
// For an IFAC packet it returns the flags, hops and ErrIFAC.
func Parse(b []byte) (*Packet, error) {
	if len(b) < 2 {
		return nil, ErrShort
	}
	if len(b) > MTU {
		return nil, ErrLong
	}
	f := b[0]
	p := &Packet{
		Raw:         b,
		Flags:       f,
		Hops:        b[1],
		Header2:     f&flagHeader2 != 0,
		ContextFlag: f&flagContext != 0,
		Transport:   f&flagTransport != 0,
		DestType:    DestType((f & maskDestType) >> 2),
		Type:        PacketType(f & maskPacketType),
	}
	if f&flagIFAC != 0 {
		return p, ErrIFAC
	}
	at := 2
	if p.Header2 {
		if len(b) < headerMinLen+HashLen {
			return nil, ErrShort
		}
		p.TransportID = b[at : at+HashLen]
		at += HashLen
	}
	if len(b) < at+HashLen+1 {
		return nil, ErrShort
	}
	p.Dest = b[at : at+HashLen]
	at += HashLen
	p.Context = b[at]
	p.Data = b[at+1:]
	return p, nil
}

// HashablePart is what Reticulum hashes to identify a packet: the low nibble
// of the flags, then everything after the hops byte and any transport ID.
// It is the same for the original and every relayed copy.
func (p *Packet) HashablePart() []byte {
	skip := 2
	if p.Header2 {
		skip += HashLen
	}
	out := make([]byte, 0, len(p.Raw)-skip+1)
	out = append(out, p.Flags&maskHashableFlag)
	return append(out, p.Raw[skip:]...)
}

// Hash is the full packet hash (Reticulum's packet.get_hash()).
func (p *Packet) Hash() [32]byte { return sha256.Sum256(p.HashablePart()) }

// TruncatedHash is the first 16 bytes of Hash. A PROOF for this packet is
// addressed to it, and a link ID is the truncated hash of its link request.
func (p *Packet) TruncatedHash() []byte {
	h := p.Hash()
	return h[:HashLen]
}

// Contexts that matter for proofs.
const (
	ContextResourceProof = 0x05
	ContextLRProof       = 0xFF
)

// ProvedHash is the truncated hash of the packet a PROOF proves, or nil if
// this is not a proof or the proof does not name its packet:
//   - SINGLE destination (an explicit proof, e.g. of a probe): the proof is
//     addressed to the proved packet's truncated hash;
//   - LINK destination with context LRPROOF: addressed to the link ID, which
//     is the link request's truncated hash;
//   - other LINK proofs: not encrypted, and the data starts with the proved
//     packet's full hash (RNS Link.prove_packet), then the signature;
//   - resource proofs name a resource, not a packet.
func (p *Packet) ProvedHash() []byte {
	if p.Type != TypeProof {
		return nil
	}
	switch {
	case p.DestType == DestSingle:
		return p.Dest
	case p.DestType == DestLink && p.Context == ContextLRProof:
		return p.Dest
	case p.DestType == DestLink && p.Context != ContextResourceProof && len(p.Data) >= 32+64:
		return p.Data[:HashLen]
	}
	return nil
}

func (p *Packet) String() string {
	s := fmt.Sprintf("%s %s hops=%d dst=%x", p.Type, p.DestType, p.Hops, p.Dest)
	if p.Header2 {
		s += fmt.Sprintf(" via=%x", p.TransportID)
	}
	return s
}

// NameHash is the 10-byte hash of a full destination name such as
// "rnstransport.path.request".
func NameHash(fullName string) []byte {
	h := sha256.Sum256([]byte(fullName))
	return h[:10]
}

// PlainDestHash is the destination hash of a PLAIN destination (no identity).
func PlainDestHash(fullName string) []byte {
	h := sha256.Sum256(NameHash(fullName))
	return h[:HashLen]
}

// Well-known PLAIN destinations by destination hash, and well-known announced
// names by name hash, for labelling traffic.
var (
	wellKnownPlain = map[string]string{}
	wellKnownNames = map[string]string{}
)

func init() {
	for _, n := range []string{
		"rnstransport.path.request",
		"rnstransport.tunnel.synthesize",
	} {
		wellKnownPlain[string(PlainDestHash(n))] = n
	}
	for _, n := range []string{
		"rnstransport.probe",
		"rnstransport.remote.management",
		"rnstransport.discovery.interface",
		"rnstransport.info.blackhole",
		"lxmf.delivery",
		"lxmf.propagation",
		"nomadnetwork.node",
	} {
		wellKnownNames[string(NameHash(n))] = n
	}
}

// WellKnownPlain names a well-known PLAIN destination hash, or "".
func WellKnownPlain(dest []byte) string { return wellKnownPlain[string(dest)] }

// WellKnownName names an announce's 10-byte name hash, or "".
func WellKnownName(nameHash []byte) string { return wellKnownNames[string(nameHash)] }
