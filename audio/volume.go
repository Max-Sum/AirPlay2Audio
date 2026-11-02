package audio

// AirPlay senders send volume as -30 (lowest) to 0 (full), or -144 for mute.
// Using that number directly as decibels leaves only 30 dB between the lowest
// and highest step, so the lowest step is still loud. VolumeToAttenuation
// spreads it over rangeDB with shairport-sync's vol2attn curve: a gentle slope
// near full volume and steeper slopes towards the bottom.

// DefaultVolumeRange is the attenuation, in dB, at the lowest volume step.
const DefaultVolumeRange = 60.0

// VolumeToAttenuation converts an AirPlay volume to the attenuation in dB
// (0 or negative) to apply. Mute and out-of-range values give -144 (silence).
func VolumeToAttenuation(volume, rangeDB float64) float64 {
	if volume > 0 || volume < -30 || rangeDB <= 0 {
		return -144
	}
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

// SetVolumeRange sets the attenuation range used for sender volume. Call
// before Run.
func (p *Player) SetVolumeRange(rangeDB float64) { p.volumeRange = rangeDB }
