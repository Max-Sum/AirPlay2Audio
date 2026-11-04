package audio

import (
	"fmt"
	"math"
)

// AirPlay senders send volume as -30 (lowest) to 0 (full), or -144 for mute.
// Using that number directly as decibels fixes the lowest step at -30 dB and
// the curve at equal steps. VolumeToAttenuation instead maps the sender's
// range onto 0 dB .. minDB along a chosen taper, after shairport-sync's
// volume control profiles.

// DefaultVolumeMin is the attenuation, in dB, at the lowest volume step.
const DefaultVolumeMin = -30.0

// VolumeTaper is the shape of the curve from full volume to the minimum.
type VolumeTaper int

const (
	// TaperStandard is shairport-sync's vol2attn curve: a gentle slope near
	// full volume and steeper slopes towards the bottom.
	TaperStandard VolumeTaper = iota
	// TaperFlat gives every step the same number of dB.
	TaperFlat
	// TaperDASL lowers the volume 10 dB, about half as loud, each time the
	// slider position halves (shairport-sync's dasl_tapered). Positions
	// below the minimum stay at the minimum.
	TaperDASL
)

var taperNames = []string{"standard", "flat", "dasl"}

func (t VolumeTaper) String() string {
	if t >= 0 && int(t) < len(taperNames) {
		return taperNames[t]
	}
	return fmt.Sprintf("VolumeTaper(%d)", int(t))
}

// ParseVolumeTaper reads a taper name: standard, flat or dasl.
func ParseVolumeTaper(name string) (VolumeTaper, error) {
	for i, n := range taperNames {
		if name == n {
			return VolumeTaper(i), nil
		}
	}
	return 0, fmt.Errorf("volume taper must be standard, flat or dasl, not %q", name)
}

// VolumeToAttenuation converts an AirPlay volume to the attenuation in dB
// (0 or negative) to apply, reaching minDB (negative) at the lowest step.
// Mute and out-of-range values give -144 (silence).
func VolumeToAttenuation(volume, minDB float64, taper VolumeTaper) float64 {
	if volume > 0 || volume < -30 || minDB >= 0 {
		return -144
	}
	switch taper {
	case TaperFlat:
		return minDB * volume / -30
	case TaperDASL:
		position := 1 - volume/-30
		if position <= 0 {
			return minDB
		}
		return math.Max(minDB, 10*math.Log2(position))
	}
	rangeDB := -minDB
	firstSlope := -rangeDB / 2
	lines := [3][2]float64{
		{0, firstSlope},
		{-5, firstSlope - (rangeDB+firstSlope)/2},
		{-17, -rangeDB},
	}
	attenuation := 0.0
	for _, line := range lines {
		if volume <= line[0] {
			if value := line[1] * (volume - line[0]) / (-30 - line[0]); value < attenuation {
				attenuation = value
			}
		}
	}
	return attenuation
}

// SetVolumeCurve sets the attenuation at the lowest sender volume step and
// the taper towards it. Call before Run.
func (p *Player) SetVolumeCurve(minDB float64, taper VolumeTaper) {
	p.volumeMin = minDB
	p.volumeTaper = taper
}
