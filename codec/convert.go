package codec

import (
	"encoding/binary"
	"math"
	"time"
)

// Blocks taken from the player last blockSize frames at blockRate, whatever
// the actual rate, so queue depths do not depend on the format.
const (
	blockSize = 512
	blockRate = 44100
)

// blockFrames is the number of frames in one block at rate.
func blockFrames(rate int) int { return blockSize * rate / blockRate }

// converter turns the player's int32 stereo into the output format: gain in
// floating point, resampling when the rates differ, then quantization to
// 16, 24 or 32 bits with optional TPDF dither.
type converter struct {
	bits      int
	dither    string
	deep      bool       // the source has more bits than the output
	resampler *resampler // nil when the rates match
	source    []int32    // one block from the player
	mix       []float64
	rng       uint64
}

func newConverter(out Format, dither string, in Format) *converter {
	c := &converter{bits: out.Bits, dither: dither, deep: in.Bits > out.Bits,
		source: make([]int32, blockFrames(in.Rate)*2), rng: 0x9e3779b97f4a7c15}
	if in.Rate != out.Rate {
		c.resampler = newResampler(in.Rate, out.Rate)
	}
	return c
}

// pendingSourceFrames is the source audio already taken from the player but
// not yet written out, which delays everything the player produces next.
func (c *converter) pendingSourceFrames() float64 {
	if c.resampler == nil {
		return 0
	}
	return c.resampler.pending()
}

// fill produces frames output frames into out (interleaved stereo bytes),
// calling pull for source blocks as needed.
func (c *converter) fill(out []byte, frames int, gain float64, pull func([]int32)) {
	if cap(c.mix) < frames*2 {
		c.mix = make([]float64, frames*2)
	}
	mix := c.mix[:frames*2]
	if c.resampler == nil {
		// One source block per output block; the rates match.
		if cap(c.source) < frames*2 {
			c.source = make([]int32, frames*2)
		}
		src := c.source[:frames*2]
		clear(src)
		pull(src)
		for i, v := range src {
			mix[i] = float64(v) / FullScale
		}
	} else {
		c.resampler.read(mix, func() []int32 {
			clear(c.source)
			pull(c.source)
			return c.source
		})
	}
	modified := gain != 1 || c.resampler != nil
	if gain != 1 {
		for i := range mix {
			mix[i] *= gain
		}
	}
	dither := c.dither == "on" || c.dither == "auto" && c.bits == 16 && (modified || c.deep)
	c.quantize(out, mix, dither)
}

func (c *converter) quantize(out []byte, mix []float64, dither bool) {
	scale := math.Ldexp(1, c.bits-1)
	max, min := scale-1, -scale
	bytes := c.bits / 8
	for i, v := range mix {
		v *= scale
		if dither {
			v += c.tpdf()
		}
		v = math.Round(v)
		if v > max {
			v = max
		} else if v < min {
			v = min
		}
		s := int64(v)
		o := out[i*bytes:]
		for b := 0; b < bytes; b++ {
			o[b] = byte(s >> (8 * b))
		}
	}
}

// tpdf returns triangular noise of +/-1 LSB peak, the usual dither for
// requantization. xorshift is plenty for noise.
func (c *converter) tpdf() float64 {
	next := func() float64 {
		c.rng ^= c.rng << 13
		c.rng ^= c.rng >> 7
		c.rng ^= c.rng << 17
		return float64(c.rng>>11) / (1 << 53)
	}
	return next() - next()
}

// resampler converts interleaved stereo with a windowed-sinc kernel. When
// it downsamples, the cutoff follows the lower rate.
type resampler struct {
	step   float64 // source frames per output frame
	pos    float64 // read position in history, in source frames
	hist   []float64
	taps   int // per side
	phases int
	table  [][]float64
}

func newResampler(from, to int) *resampler {
	r := &resampler{step: float64(from) / float64(to), taps: 16, phases: 512}
	cutoff := 0.5 * 0.91 // of the source rate, leaving a transition band below its Nyquist frequency
	if to < from {
		cutoff *= float64(to) / float64(from)
	}
	const beta = 8.0
	bessel := func(x float64) float64 {
		sum, term := 1.0, 1.0
		for k := 1; k < 30; k++ {
			term *= (x / 2 / float64(k)) * (x / 2 / float64(k))
			sum += term
		}
		return sum
	}
	norm := bessel(beta)
	r.table = make([][]float64, r.phases+1)
	for p := 0; p <= r.phases; p++ {
		frac := float64(p) / float64(r.phases)
		coeffs := make([]float64, 2*r.taps)
		sum := 0.0
		for k := range coeffs {
			// Tap k weights source frame floor(pos)-taps+1+k.
			t := float64(k-r.taps+1) - frac
			x := 2 * cutoff * t
			sinc := 1.0
			if x != 0 {
				sinc = math.Sin(math.Pi*x) / (math.Pi * x)
			}
			w := t / float64(r.taps)
			window := 0.0
			if w > -1 && w < 1 {
				window = bessel(beta*math.Sqrt(1-w*w)) / norm
			}
			coeffs[k] = 2 * cutoff * sinc * window
			sum += coeffs[k]
		}
		for k := range coeffs {
			coeffs[k] /= sum
		}
		r.table[p] = coeffs
	}
	// Start with taps frames of silence so the first output frame has history.
	r.hist = make([]float64, 2*r.taps)
	r.pos = float64(r.taps - 1)
	return r
}

func (r *resampler) pending() float64 { return float64(len(r.hist)/2) - r.pos }

func (r *resampler) read(out []float64, more func() []int32) {
	for i := 0; i+1 < len(out); i += 2 {
		for !r.ready() {
			for _, v := range more() {
				r.hist = append(r.hist, float64(v)/FullScale)
			}
		}
		out[i], out[i+1] = r.next()
	}
	r.trim()
}

// ready reports whether the history reaches far enough for the next frame.
func (r *resampler) ready() bool { return int(r.pos)+r.taps < len(r.hist)/2 }

// next computes one output frame and advances; ready must be true.
func (r *resampler) next() (float64, float64) {
	base := int(r.pos)
	frac := r.pos - float64(base)
	ph := frac * float64(r.phases)
	p := int(ph)
	mixWeight := ph - float64(p)
	a, b := r.table[p], r.table[p+1]
	start := (base - r.taps + 1) * 2
	var left, right float64
	for k := range a {
		c := a[k] + (b[k]-a[k])*mixWeight
		left += c * r.hist[start+2*k]
		right += c * r.hist[start+2*k+1]
	}
	r.pos += r.step
	return left, right
}

// trim drops history no longer reachable by the kernel.
func (r *resampler) trim() {
	if drop := int(r.pos) - r.taps + 1; drop > 4096 {
		r.hist = append(r.hist[:0], r.hist[drop*2:]...)
		r.pos -= float64(drop)
	}
}

// fixedOutput plays a Source on a device opened at one format, converting
// whatever the source produces. It is for outputs that do not reopen their
// device when the stream's format changes.
type fixedOutput struct {
	source Source
	out    Format
	in     Format
	conv   *converter
	buf    []byte
}

func newFixedOutput(source Source, out Format) *fixedOutput {
	return &fixedOutput{source: source, out: out}
}

// read fills out with 16-bit stereo at the device rate. The times are
// passed on to the source.
func (f *fixedOutput) read(out []int16, currentTime, outputBufferDacTime time.Duration) {
	if in := f.source.Format(); f.conv == nil || in != f.in {
		f.in, f.conv = in, newConverter(f.out, "auto", in)
	}
	frames := len(out) / 2
	if cap(f.buf) < frames*4 {
		f.buf = make([]byte, frames*4)
	}
	buf := f.buf[:frames*4]
	f.conv.fill(buf, frames, 1, func(src []int32) { f.source.Fill(src, currentTime, outputBufferDacTime) })
	for i := range out[:frames*2] {
		out[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
	}
}
