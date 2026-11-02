package codec

import (
	"encoding/binary"
	"math"
)

// downmixToStereo mixes interleaved 16-bit PCM in WAVE channel order (front
// left, front right, center, LFE, then left/right surround pairs) to stereo.
// Center and surrounds are added at -3 dB, LFE is dropped, and the result is
// scaled so that full-scale input in every channel cannot clip.
func downmixToStereo(pcm []byte, channels int) []byte {
	if channels <= 2 {
		return pcm
	}
	const side = math.Sqrt2 / 2
	pairs := 0
	if channels > 4 {
		pairs = (channels - 4) / 2
	}
	scale := 1 / (1 + side*float64(1+pairs)) // front + center + surrounds
	frames := len(pcm) / (2 * channels)
	out := make([]byte, frames*4)
	sample := func(frame, channel int) float64 {
		return float64(int16(binary.LittleEndian.Uint16(pcm[(frame*channels+channel)*2:])))
	}
	for i := 0; i < frames; i++ {
		center := side * sample(i, 2)
		left, right := sample(i, 0)+center, sample(i, 1)+center
		for p := 0; p < pairs; p++ {
			left += side * sample(i, 4+2*p)
			right += side * sample(i, 5+2*p)
		}
		binary.LittleEndian.PutUint16(out[i*4:], uint16(clampInt16(left*scale)))
		binary.LittleEndian.PutUint16(out[i*4+2:], uint16(clampInt16(right*scale)))
	}
	return out
}

func clampInt16(v float64) int16 {
	v = math.Round(v)
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(v)
}
