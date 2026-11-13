package reticulum

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Human-readable information carried in announce app data.

// Discovery is an interface-discovery announce (rnstransport.discovery.interface).
// Key numbers are RNS Discovery.py's.
type Discovery struct {
	Encrypted   bool // map encrypted to a network identity: nothing else is readable
	Name        string
	Interface   string // e.g. RNodeInterface, TCPServerInterface
	Transport   bool
	TransportID []byte
	Impl        string // e.g. RNS, Rrptr
	Version     string
	ReachableOn string // host for IP interfaces
	Port        int64
	Lat, Lon    *float64
	Height      *float64
	FreqHz      int64
	BwHz        int64
	SF, CR      int64
	IFACNet     string // IFAC network name, if announced
}

const discoveryStampLen = 32

// ParseDiscovery reads interface-discovery app data:
// flags (1) || msgpack map || stamp (32). Flag 0x02 means encrypted.
func ParseDiscovery(appData []byte) (*Discovery, error) {
	if len(appData) < 1+discoveryStampLen+1 {
		return nil, ErrShort
	}
	d := &Discovery{}
	if appData[0]&0x02 != 0 {
		d.Encrypted = true
		return d, nil
	}
	v, _, err := msgpackDecode(appData[1 : len(appData)-discoveryStampLen])
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[any]any)
	if !ok {
		return nil, errMsgpack
	}
	get := func(k int64) any { return m[k] }
	d.Name = clean(str(get(0xFF)))
	d.Interface = clean(str(get(0x00)))
	d.Transport, _ = get(0x01).(bool)
	d.TransportID, _ = get(0xFE).([]byte)
	d.Impl = clean(str(get(0xFD)))
	d.Version = clean(str(get(0xFC)))
	d.ReachableOn = clean(str(get(0x02)))
	d.Port, _ = integer(get(0x06))
	d.Lat, d.Lon, d.Height = float(get(0x03)), float(get(0x04)), float(get(0x05))
	if d.Lat != nil && (*d.Lat < -90 || *d.Lat > 90) || d.Lon != nil && (*d.Lon < -180 || *d.Lon > 180) {
		d.Lat, d.Lon = nil, nil // not a position
	}
	if (d.Lat == nil) != (d.Lon == nil) {
		d.Lat, d.Lon = nil, nil
	}
	d.FreqHz, _ = integer(get(0x09))
	d.BwHz, _ = integer(get(0x0A))
	d.SF, _ = integer(get(0x0B))
	d.CR, _ = integer(get(0x0C))
	d.IFACNet = clean(str(get(0x07)))
	return d, nil
}

func str(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		if utf8.Valid(s) {
			return string(s)
		}
	}
	return ""
}

func integer(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func float(v any) *float64 {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int64:
		f = float64(n)
	case uint64:
		f = float64(n)
	default:
		return nil
	}
	return &f
}

// AnnounceInfo is what an announce says about itself in human terms.
type AnnounceInfo struct {
	Name      string     // a display name, if the app data carries one
	Summary   string     // one line for the live view
	Discovery *Discovery // set for interface-discovery announces
}

// AppLabel is a short, human name for a well-known announce app, or "".
func AppLabel(nameHash []byte) string {
	switch WellKnownName(nameHash) {
	case "rnstransport.discovery.interface":
		return "Discovery"
	case "nomadnetwork.node":
		return "NomadNet"
	case "lxmf.delivery":
		return "LXMF"
	case "lxmf.propagation":
		return "LXMF propagation"
	case "rnstransport.probe":
		return "Probe"
	case "rnstransport.remote.management":
		return "Remote management"
	case "rnstransport.info.blackhole":
		return "Blackhole list"
	}
	return ""
}

// DescribeAnnounce summarises an announce's app data by its app (name hash).
// The summary starts with the app's label (Discovery, NomadNet, LXMF...).
// Announces are public broadcasts, so a discovery position is shown in full.
func DescribeAnnounce(nameHash, appData []byte) AnnounceInfo {
	label := AppLabel(nameHash)
	if label == "" {
		label = "Announce"
	}
	parts := []string{label}
	var info AnnounceInfo
	done := func() AnnounceInfo {
		info.Summary = strings.Join(parts, " · ")
		return info
	}
	switch WellKnownName(nameHash) {
	case "rnstransport.discovery.interface":
		d, err := ParseDiscovery(appData)
		if err != nil {
			parts = append(parts, "data unreadable")
			return done()
		}
		info.Discovery = d
		if d.Encrypted {
			parts = append(parts, "encrypted to a network")
			return done()
		}
		info.Name = d.Name
		if d.Name != "" {
			parts = append(parts, fmt.Sprintf("%q", d.Name))
		}
		if d.Impl != "" {
			parts = append(parts, strings.TrimSpace(d.Impl+" "+d.Version))
		}
		if d.FreqHz > 0 {
			radio := fmt.Sprintf("%.3f MHz", float64(d.FreqHz)/1e6)
			if d.BwHz > 0 {
				radio += fmt.Sprintf(" %g kHz", float64(d.BwHz)/1e3)
			}
			if d.SF > 0 {
				radio += fmt.Sprintf(" SF%d", d.SF)
			}
			if d.CR > 0 {
				radio += fmt.Sprintf(" CR4/%d", d.CR)
			}
			parts = append(parts, radio)
		}
		if d.ReachableOn != "" {
			ep := d.ReachableOn
			if d.Port > 0 {
				ep += fmt.Sprintf(":%d", d.Port)
			}
			parts = append(parts, "reachable on "+ep)
		}
		if d.IFACNet != "" {
			parts = append(parts, fmt.Sprintf("network %q", d.IFACNet))
		}
		if d.Lat != nil {
			parts = append(parts, fmt.Sprintf("%.5f, %.5f", *d.Lat, *d.Lon))
		}
		if d.Height != nil {
			parts = append(parts, fmt.Sprintf("%.0f m", *d.Height))
		}
		return done()
	case "lxmf.delivery":
		// [display name, stamp cost] (older versions: the name as plain bytes)
		info.Name = DisplayName(nameHash, appData)
		if info.Name != "" {
			parts = append(parts, fmt.Sprintf("%q", info.Name))
		}
		if v, _, err := msgpackDecode(appData); err == nil {
			if a, ok := v.([]any); ok && len(a) > 1 {
				if c, ok := integer(a[1]); ok {
					parts = append(parts, fmt.Sprintf("stamp cost %d", c))
				}
			}
		}
		return done()
	case "lxmf.propagation":
		return done()
	}
	if len(appData) == 0 {
		return done()
	}
	if name := DisplayName(nameHash, appData); name != "" {
		info.Name = name
		parts = append(parts, fmt.Sprintf("%q", name))
		return done()
	}
	other := fmt.Sprintf("%d bytes of app data", len(appData))
	if len(appData) <= 8 {
		other += " " + hex.EncodeToString(appData)
	}
	parts = append(parts, other)
	return done()
}
