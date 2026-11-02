package codec

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestConverterIsBitExactAtUnityGain(t *testing.T) {
	c := newConverter(OutputConfig{Bits: 16, Rate: SourceRate, Dither: "auto"}, 4)
	out := make([]byte, 4*4)
	c.fill(out, 4, 1, func(src []int16) { copy(src, []int16{1, -1, 32767, -32768, 0, 5, -5, 100}) })
	want := []int16{1, -1, 32767, -32768, 0, 5, -5, 100}
	for i, w := range want {
		if got := int16(binary.LittleEndian.Uint16(out[i*2:])); got != w {
			t.Fatalf("sample %d = %d, want %d", i, got, w)
		}
	}
}

func TestConverterWidensTo24And32Bits(t *testing.T) {
	for _, bits := range []int{24, 32} {
		c := newConverter(OutputConfig{Bits: bits, Rate: SourceRate, Dither: "auto"}, 1)
		out := make([]byte, 2*bits/8)
		c.fill(out, 1, 0.5, func(src []int16) { src[0], src[1] = 1, -32768 })
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
	c := newConverter(OutputConfig{Bits: 16, Rate: SourceRate, Dither: "auto"}, 64)
	out := make([]byte, 64*4)
	c.fill(out, 64, 0.5, func(src []int16) {})
	nonzero := 0
	for i := 0; i < 128; i++ {
		if binary.LittleEndian.Uint16(out[i*2:]) != 0 {
			nonzero++
		}
	}
	if nonzero == 0 {
		t.Fatal("expected dither noise on scaled 16-bit output")
	}
	c = newConverter(OutputConfig{Bits: 16, Rate: SourceRate, Dither: "off"}, 64)
	c.fill(out, 64, 0.5, func(src []int16) {})
	for i := 0; i < 128; i++ {
		if binary.LittleEndian.Uint16(out[i*2:]) != 0 {
			t.Fatal("dither=off produced noise from silence")
		}
	}
}

// A 1 kHz tone resampled to 48 kHz keeps its frequency and level.
func TestResamplerPreservesTone(t *testing.T) {
	r := newResampler(44100, 48000)
	n := 0
	more := func() []int16 {
		block := make([]int16, 512*2)
		for i := 0; i < 512; i++ {
			v := int16(16000 * math.Sin(2*math.Pi*1000*float64(n)/44100))
			block[2*i], block[2*i+1] = v, v
			n++
		}
		return block
	}
	out := make([]float64, 48000*2)
	r.read(out, more)
	// Skip the start-up transient, then correlate against 1 kHz at 48 kHz.
	var sumSin, sumCos, energy float64
	for i := 4800; i < 48000; i++ {
		x := out[2*i]
		sumSin += x * math.Sin(2*math.Pi*1000*float64(i)/48000)
		sumCos += x * math.Cos(2*math.Pi*1000*float64(i)/48000)
		energy += x * x
	}
	count := float64(48000 - 4800)
	amplitude := 2 * math.Hypot(sumSin, sumCos) / count
	if math.Abs(amplitude-16000.0/32768) > 0.01 {
		t.Fatalf("amplitude %.4f, want %.4f", amplitude, 16000.0/32768)
	}
	// Nearly all energy is at 1 kHz.
	if tone := amplitude * amplitude / 2 * count; tone/energy < 0.999 {
		t.Fatalf("distortion too high: %.5f of energy in tone", tone/energy)
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
