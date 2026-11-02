package audio

import (
	"math"
	"testing"
)

func TestVolumeToAttenuation(t *testing.T) {
	// For a 60 dB range shairport-sync's vol2attn uses the lines
	// {0, -30}, {-5, -45} and {-17, -60} and takes the lowest.
	for _, tc := range []struct {
		volume, rangeDB, want float64
	}{
		{0, 60, 0},
		{-5, 60, -5},
		{-15, 60, -18},
		{-20, 60, -27},
		{-30, 60, -60},
		{-30, 96, -96},
		{-144, 60, -144},
		{5, 60, -144},
	} {
		if got := VolumeToAttenuation(tc.volume, tc.rangeDB); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("VolumeToAttenuation(%v, %v) = %v, want %v", tc.volume, tc.rangeDB, got, tc.want)
		}
	}
	previous := 1.0
	for v := 0.0; v >= -30; v -= 0.25 {
		got := VolumeToAttenuation(v, 60)
		if got > previous {
			t.Fatalf("not monotonic at %v: %v after %v", v, got, previous)
		}
		previous = got
	}
}
