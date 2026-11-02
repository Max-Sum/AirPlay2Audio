package audio

import (
	"goplay2/codec"
	"goplay2/ptp"
)

// NewPlayerWithOutput is NewPlayer with another audio output, so tests can
// drive the whole receiver without a sound card.
func NewPlayerWithOutput(clock *ptp.VirtualClock, ring *Ring, output codec.Stream) *Player {
	p := NewPlayer(clock, ring)
	p.stream = output
	return p
}
