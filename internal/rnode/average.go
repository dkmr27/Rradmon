package rnode

import (
	"math"
	"time"
)

// Channel report intervals: the RNode reports about once a second, which
// is far more than the noise floor and utilisation history need.
const (
	ChannelIntervalDefault = time.Minute
	ChannelIntervalMin     = time.Minute
	ChannelIntervalMax     = 10 * time.Minute
)

// ChannelAverager turns the RNode's channel reports into one per interval.
// The RNode's short-term utilisation covers only the last few seconds, so a
// single sample would miss most of the interval: UtilShort and NoiseFloor
// are averaged over it, Interference is the strongest seen, and the other
// fields come from the last report.
type ChannelAverager struct {
	Interval time.Duration

	start          time.Time
	n              int
	sumUtil, sumNF float64
	last           ChannelStats
	interf         *float32
}

// Add takes one report and returns the summary when the interval is over.
func (a *ChannelAverager) Add(cs ChannelStats) (ChannelStats, bool) {
	if a.start.IsZero() {
		a.start = cs.T
	}
	a.n++
	a.sumUtil += float64(cs.UtilShort)
	a.sumNF += float64(cs.NoiseFloor)
	a.last = cs
	if cs.Interference != nil && (a.interf == nil || *cs.Interference > *a.interf) {
		v := *cs.Interference
		a.interf = &v
	}
	if cs.T.Sub(a.start) < a.Interval {
		return ChannelStats{}, false
	}
	out := a.last
	out.UtilShort = round(a.sumUtil/float64(a.n), 100)
	out.NoiseFloor = round(a.sumNF/float64(a.n), 10)
	out.Interference = a.interf
	// The next interval starts where this one ended, so they do not drift.
	*a = ChannelAverager{Interval: a.Interval, start: cs.T}
	return out, true
}

func round(v, scale float64) float32 { return float32(math.Round(v*scale) / scale) }
