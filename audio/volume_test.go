package audio

import (
	"math"
	"testing"
)

func TestVolumeToAttenuation(t *testing.T) {
	for _, tc := range []struct {
		volume, minDB float64
		taper         VolumeTaper
		want          float64
	}{
		// For -60 dB shairport-sync's vol2attn uses the lines
		// {0, -30}, {-5, -45} and {-17, -60} and takes the lowest.
		{0, -60, TaperStandard, 0},
		{-5, -60, TaperStandard, -5},
		{-15, -60, TaperStandard, -18},
		{-20, -60, TaperStandard, -27},
		{-30, -60, TaperStandard, -60},
		{-30, -96, TaperStandard, -96},
		// For -30 dB the lines are {0, -15}, {-5, -22.5} and {-17, -30}.
		{-5, -30, TaperStandard, -2.5},
		{-30, -30, TaperStandard, -30},
		{-15, -30, TaperFlat, -15},
		{-30, -40, TaperFlat, -40},
		{-15, -30, TaperDASL, -10},
		{-22.5, -30, TaperDASL, -20},
		{-28, -30, TaperDASL, -30},
		{-30, -30, TaperDASL, -30},
		{-144, -30, TaperStandard, -144},
		{-144, -30, TaperDASL, -144},
		{5, -30, TaperFlat, -144},
	} {
		if got := VolumeToAttenuation(tc.volume, tc.minDB, tc.taper); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("VolumeToAttenuation(%v, %v, %v) = %v, want %v", tc.volume, tc.minDB, tc.taper, got, tc.want)
		}
	}
	for _, taper := range []VolumeTaper{TaperStandard, TaperFlat, TaperDASL} {
		previous := 1.0
		for v := 0.0; v >= -30; v -= 0.25 {
			got := VolumeToAttenuation(v, -30, taper)
			if got > previous || got < -30 {
				t.Fatalf("%v: %v at %v after %v", taper, got, v, previous)
			}
			previous = got
		}
	}
}

func TestParseVolumeTaper(t *testing.T) {
	for _, name := range []string{"standard", "flat", "dasl"} {
		taper, err := ParseVolumeTaper(name)
		if err != nil || taper.String() != name {
			t.Errorf("ParseVolumeTaper(%q) = %v, %v", name, taper, err)
		}
	}
	if _, err := ParseVolumeTaper("log"); err == nil {
		t.Error("ParseVolumeTaper accepted an unknown name")
	}
}
