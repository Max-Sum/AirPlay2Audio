package audio

import "github.com/Max-Sum/AirPlay2Audio/codec"

// RealtimeFormat is the encoding of an AirPlay 2 realtime (type 96) stream,
// which is always ALAC stereo.
type RealtimeFormat struct {
	Rate int
	Bits int
}

// CDRealtime is the realtime format of a SETUP that names none.
var CDRealtime = RealtimeFormat{Rate: cdRate, Bits: 16}

func (f RealtimeFormat) codec() codec.Format { return codec.Format{Rate: f.Rate, Bits: f.Bits} }

// RealtimeFormatFor returns the realtime format named by a SETUP audioFormat
// and sample rate (sr); either may be 0. It reports false for a format this
// receiver does not decode, or a rate that contradicts the format.
func RealtimeFormatFor(audioFormat uint64, sampleRate uint32) (RealtimeFormat, bool) {
	var f RealtimeFormat
	switch audioFormat {
	case 0:
		// No format named: 16-bit at the given rate.
		f = RealtimeFormat{Rate: int(sampleRate), Bits: 16}
		if f.Rate == 0 {
			f.Rate = cdRate
		}
		return f, f.Rate == 44100 || f.Rate == 48000
	case audioFormatALAC44100S16:
		f = RealtimeFormat{Rate: 44100, Bits: 16}
	case audioFormatALAC44100S24:
		f = RealtimeFormat{Rate: 44100, Bits: 24}
	case audioFormatALAC48000S16:
		f = RealtimeFormat{Rate: 48000, Bits: 16}
	case audioFormatALAC48000S24:
		f = RealtimeFormat{Rate: 48000, Bits: 24}
	default:
		return RealtimeFormat{}, false
	}
	if sampleRate != 0 && int(sampleRate) != f.Rate {
		return RealtimeFormat{}, false
	}
	return f, true
}
