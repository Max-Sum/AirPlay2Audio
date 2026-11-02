package codec

import (
	"encoding/binary"
	"math"
	"testing"
)

func pcmFrames(frames [][]int16) []byte {
	var out []byte
	for _, frame := range frames {
		for _, v := range frame {
			out = binary.LittleEndian.AppendUint16(out, uint16(v))
		}
	}
	return out
}

func stereoAt(pcm []byte, frame int) (int16, int16) {
	return int16(binary.LittleEndian.Uint16(pcm[frame*4:])), int16(binary.LittleEndian.Uint16(pcm[frame*4+2:]))
}

func TestDownmixStereoUnchanged(t *testing.T) {
	in := pcmFrames([][]int16{{100, -200}})
	if out := downmixToStereo(in, 2); &out[0] != &in[0] {
		t.Fatal("stereo input was copied")
	}
}

func TestDownmixSurround(t *testing.T) {
	// 5.1: FL FR FC LFE SL SR
	in := pcmFrames([][]int16{
		{10000, 0, 0, 0, 0, 0},
		{0, 0, 10000, 0, 0, 0},
		{0, 0, 0, 30000, 0, 0},
		{0, 0, 0, 0, 0, 10000},
		{32767, 32767, 32767, 32767, 32767, 32767},
		{-32768, -32768, -32768, -32768, -32768, -32768},
	})
	out := downmixToStereo(in, 6)
	if len(out) != 6*4 {
		t.Fatalf("got %d bytes", len(out))
	}
	scale := 1 / (1 + math.Sqrt2)
	if l, r := stereoAt(out, 0); l != int16(math.Round(10000*scale)) || r != 0 {
		t.Fatalf("front left: %d %d", l, r)
	}
	if l, r := stereoAt(out, 1); l != r || l == 0 {
		t.Fatalf("center not in both channels: %d %d", l, r)
	}
	if l, r := stereoAt(out, 2); l != 0 || r != 0 {
		t.Fatalf("LFE was mixed: %d %d", l, r)
	}
	if l, r := stereoAt(out, 3); l != 0 || r == 0 {
		t.Fatalf("right surround: %d %d", l, r)
	}
	if l, r := stereoAt(out, 4); l != 32767 || r != 32767 {
		t.Fatalf("full scale: %d %d", l, r)
	}
	if l, r := stereoAt(out, 5); l != -32768 || r != -32768 {
		t.Fatalf("negative full scale: %d %d", l, r)
	}
}

func TestDownmix71(t *testing.T) {
	// 7.1: FL FR FC LFE BL BR SL SR
	in := pcmFrames([][]int16{{0, 0, 0, 0, 0, 0, 10000, 0}, {32767, 32767, 32767, 0, 32767, 32767, 32767, 32767}})
	out := downmixToStereo(in, 8)
	if l, r := stereoAt(out, 0); l == 0 || r != 0 {
		t.Fatalf("left side surround: %d %d", l, r)
	}
	if l, r := stereoAt(out, 1); l != 32767 || r != 32767 {
		t.Fatalf("full scale: %d %d", l, r)
	}
}
