package rnode

import (
	"testing"
	"time"
)

func TestChannelAverager(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	a := ChannelAverager{Interval: time.Minute}
	i1, i2 := float32(-90), float32(-80)
	var out []ChannelStats
	// 121 reports a second apart: busy (30%) for 15 s in each minute.
	for s := 0; s <= 120; s++ {
		cs := ChannelStats{T: t0.Add(time.Duration(s) * time.Second), NoiseFloor: -104, UtilLong: float32(s) / 100}
		if s%60 >= 5 && s%60 < 20 {
			cs.UtilShort, cs.NoiseFloor = 30, -100
		}
		if s == 10 {
			cs.Interference = &i1
		}
		if s == 20 {
			cs.Interference = &i2
		}
		if c, ok := a.Add(cs); ok {
			out = append(out, c)
		}
	}
	if len(out) != 2 {
		t.Fatalf("got %d summaries, want 2 (one per minute)", len(out))
	}
	c := out[0]
	if !c.T.Equal(t0.Add(time.Minute)) {
		t.Errorf("T = %v, want the last report's time", c.T)
	}
	// 61 reports, 15 busy: 15*30/61 = 7.38 %
	if c.UtilShort != 7.38 {
		t.Errorf("UtilShort = %v, want 7.38", c.UtilShort)
	}
	if c.NoiseFloor != -103 { // (15*-100 + 46*-104)/61 = -103.02
		t.Errorf("NoiseFloor = %v, want -103", c.NoiseFloor)
	}
	if c.UtilLong != 0.6 {
		t.Errorf("UtilLong = %v, want the last report's 0.6", c.UtilLong)
	}
	if c.Interference == nil || *c.Interference != -80 {
		t.Errorf("Interference = %v, want the strongest, -80", c.Interference)
	}
	if out[1].Interference != nil {
		t.Errorf("second minute has interference %v, want none", *out[1].Interference)
	}
}
