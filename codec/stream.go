package codec

import (
	"io"
	"time"
)

// Format is a PCM sample rate and sample size.
type Format struct {
	Rate int // frames per second
	Bits int // significant bits per sample: 16 or 24
}

// DefaultFormat is used until a stream names its own.
var DefaultFormat = Format{Rate: 44100, Bits: 16}

// FullScale is the sample value of 0 dBFS. The player carries samples as
// int32, left-aligned (a 16-bit sample is shifted up by 16, a 24-bit one by
// 8), so audio of either size keeps all of its bits.
const FullScale = 1 << 31

// Source produces the audio a Stream plays.
type Source interface {
	// Format is the format of the audio the next Fill produces. A stream
	// calls it before every Fill and reconfigures itself when it changes.
	Format() Format
	// Fill writes interleaved stereo samples (see FullScale) into out.
	// outputBufferDacTime - currentTime is how long before the first of
	// them reaches the speaker.
	Fill(out []int32, currentTime, outputBufferDacTime time.Duration)
}

type Stream interface {
	io.Closer
	Init(source Source) error
	Start() error
	Stop() error
	SetVolume(volume float64) error
}
