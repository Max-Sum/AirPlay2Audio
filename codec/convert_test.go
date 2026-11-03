package codec

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

var cd = Format{Rate: 44100, Bits: 16}

// s16 is a 16-bit sample as the player carries it.
func s16(v int16) int32 { return int32(v) << 16 }

func TestConverterIsBitExactAtUnityGain(t *testing.T) {
	c := newConverter(cd, "auto", cd)
	out := make([]byte, 4*4)
	want := []int16{1, -1, 32767, -32768, 0, 5, -5, 100}
	c.fill(out, 4, 1, func(src []int32) {
		for i, v := range want {
			src[i] = s16(v)
		}
	})
	for i, w := range want {
		if got := int16(binary.LittleEndian.Uint16(out[i*2:])); got != w {
			t.Fatalf("sample %d = %d, want %d", i, got, w)
		}
	}
}

// 24-bit audio written as 24-bit keeps every bit, without dither.
func TestConverterKeeps24BitSamples(t *testing.T) {
	f := Format{Rate: 48000, Bits: 24}
	c := newConverter(f, "auto", f)
	want := []int32{1, -1, 0x7fffff, -0x800000}
	out := make([]byte, 2*3*2)
	c.fill(out, 2, 1, func(src []int32) {
		for i, v := range want {
			src[i] = v << 8
		}
	})
	for i, w := range want {
		b := out[i*3:]
		if got := int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24) >> 8; got != w {
			t.Fatalf("sample %d = %d, want %d", i, got, w)
		}
	}
}

func TestConverterWidensTo24And32Bits(t *testing.T) {
	for _, bits := range []int{24, 32} {
		c := newConverter(Format{Rate: 44100, Bits: bits}, "auto", cd)
		out := make([]byte, 2*bits/8)
		c.fill(out, 1, 0.5, func(src []int32) { src[0], src[1] = s16(1), s16(-32768) })
		read := func(i int) int64 {
			var v int64
			b := bits / 8
			for k := b - 1; k >= 0; k-- {
				v = v<<8 | int64(out[i*b+k])
			}
			return v << (64 - bits) >> (64 - bits)
		}
		// Halving a 16-bit sample keeps full precision in wider output.
		if l, r := read(0), read(1); l != int64(1)<<(bits-16)/2 || r != -(int64(1)<<(bits-2)) {
			t.Fatalf("%d-bit: got %d %d", bits, l, r)
		}
	}
}

func TestDitherOnlyWhenRequantizing(t *testing.T) {
	nonzero := func(out []byte) int {
		n := 0
		for i := 0; i+1 < len(out); i += 2 {
			if binary.LittleEndian.Uint16(out[i:]) != 0 {
				n++
			}
		}
		return n
	}
	out := make([]byte, 64*4)
	c := newConverter(cd, "auto", cd)
	c.fill(out, 64, 0.5, func(src []int32) {})
	if nonzero(out) == 0 {
		t.Fatal("expected dither noise on scaled 16-bit output")
	}
	c = newConverter(cd, "off", cd)
	c.fill(out, 64, 0.5, func(src []int32) {})
	if nonzero(out) != 0 {
		t.Fatal("dither=off produced noise from silence")
	}
	c = newConverter(cd, "auto", cd)
	c.fill(out, 64, 1, func(src []int32) {})
	if nonzero(out) != 0 {
		t.Fatal("unmodified 16-bit audio was dithered")
	}
	// 24-bit audio written as 16 bits is requantized, so it is dithered.
	c = newConverter(cd, "auto", Format{Rate: 44100, Bits: 24})
	c.fill(out, 64, 1, func(src []int32) {})
	if nonzero(out) == 0 {
		t.Fatal("expected dither noise on 24-bit audio written as 16 bits")
	}
}

// tone returns blocks of a 1 kHz stereo tone at rate.
func tone(rate int, amplitude float64) func() []int32 {
	n := 0
	return func() []int32 {
		block := make([]int32, 512*2)
		for i := 0; i < 512; i++ {
			v := int32(amplitude * FullScale * math.Sin(2*math.Pi*1000*float64(n)/float64(rate)))
			block[2*i], block[2*i+1] = v, v
			n++
		}
		return block
	}
}

// checkTone checks that out (interleaved, at rate) is a clean 1 kHz tone.
func checkTone(t *testing.T, out []float64, rate int, amplitude float64) {
	t.Helper()
	var sumSin, sumCos, energy float64
	start, end := rate/10, len(out)/2 // skip the start-up transient
	for i := start; i < end; i++ {
		x := out[2*i]
		sumSin += x * math.Sin(2*math.Pi*1000*float64(i)/float64(rate))
		sumCos += x * math.Cos(2*math.Pi*1000*float64(i)/float64(rate))
		energy += x * x
	}
	count := float64(end - start)
	got := 2 * math.Hypot(sumSin, sumCos) / count
	if math.Abs(got-amplitude) > 0.01 {
		t.Fatalf("amplitude %.4f, want %.4f", got, amplitude)
	}
	// Nearly all energy is at 1 kHz.
	if tone := got * got / 2 * count; tone/energy < 0.999 {
		t.Fatalf("distortion too high: %.5f of energy in tone", tone/energy)
	}
}

// A 1 kHz tone resampled to 48 kHz keeps its frequency and level.
func TestResamplerPreservesTone(t *testing.T) {
	r := newResampler(44100, 48000)
	out := make([]float64, 48000*2)
	r.read(out, tone(44100, 0.5))
	checkTone(t, out, 48000, 0.5)
}

// Downsampling 48 kHz to 44.1 kHz (a fixed output rate) keeps the tone too.
func TestResamplerDownsamplesTone(t *testing.T) {
	r := newResampler(48000, 44100)
	out := make([]float64, 44100*2)
	r.read(out, tone(48000, 0.5))
	checkTone(t, out, 44100, 0.5)
}

func TestResolveFollowsTheStream(t *testing.T) {
	follow := OutputConfig{}
	cases := []struct{ in, want Format }{
		{Format{44100, 16}, Format{44100, 16}},
		{Format{48000, 16}, Format{48000, 16}},
		{Format{48000, 24}, Format{48000, 24}},
	}
	for _, c := range cases {
		if got := follow.Resolve(c.in); got != c.want {
			t.Errorf("follow %v gave %v, want %v", c.in, got, c.want)
		}
	}
	fixed := OutputConfig{Rate: 96000, Bits: 32}
	if got := fixed.Resolve(Format{48000, 24}); got != (Format{96000, 32}) {
		t.Errorf("fixed format gave %v", got)
	}
	if got := (OutputConfig{Rate: 48000}).Resolve(Format{44100, 24}); got != (Format{48000, 24}) {
		t.Errorf("fixed rate, following bits gave %v", got)
	}
}

type formatSource struct {
	format Format
	fills  int
}

func (s *formatSource) Format() Format { return s.format }
func (s *formatSource) Fill(out []int32, _, _ time.Duration) {
	s.fills++
	for i := range out {
		out[i] = s16(1000)
	}
}

// A fixed-format output converts a stream whose format changes.
func TestFixedOutputFollowsSourceFormat(t *testing.T) {
	src := &formatSource{format: cd}
	f := newFixedOutput(src, cd)
	out := make([]int16, 512*2)
	f.read(out, 0, 0)
	if out[0] != 1000 || out[len(out)-1] != 1000 {
		t.Fatalf("44.1 kHz passthrough gave %d..%d", out[0], out[len(out)-1])
	}
	src.format = Format{Rate: 48000, Bits: 24}
	for i := 0; i < 20; i++ {
		f.read(out, 0, 0)
	}
	if f.conv.resampler == nil || f.in != src.format {
		t.Fatal("converter did not follow the new format")
	}
	if v := out[len(out)-1]; v < 990 || v > 1010 {
		t.Fatalf("resampled constant gave %d", v)
	}
}

func TestMatchDevice(t *testing.T) {
	devices := []Device{{0, "Speakers (Realtek(R) Audio)"}, {1, "Headphones (USB DAC)"}, {2, "Speakers (USB DAC)"}}
	if d, err := matchDevice("1", devices); err != nil || d.Index != 1 {
		t.Fatalf("index: %v %v", d, err)
	}
	if d, err := matchDevice("realtek", devices); err != nil || d.Index != 0 {
		t.Fatalf("substring: %v %v", d, err)
	}
	if _, err := matchDevice("speakers", devices); err == nil {
		t.Fatal("ambiguous name accepted")
	}
	if _, err := matchDevice("9", devices); err == nil {
		t.Fatal("missing index accepted")
	}
}
