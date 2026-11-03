package codec

import "math"

// converter turns the player's 44.1 kHz int16 stereo into the configured
// output format: gain in floating point, optional resampling, then
// quantization to 16, 24 or 32 bits with optional TPDF dither.
type converter struct {
	bits      int
	dither    string
	resampler *resampler // nil at 44.1 kHz
	source    []int16    // one block from the player
	mix       []float64
	rng       uint64
}

func newConverter(cfg OutputConfig, sourceFrames int) *converter {
	c := &converter{bits: cfg.Bits, dither: cfg.Dither, source: make([]int16, sourceFrames*2), rng: 0x9e3779b97f4a7c15}
	if cfg.Rate != SourceRate {
		c.resampler = newResampler(SourceRate, cfg.Rate)
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
// calling pull for 44.1 kHz source blocks as needed.
func (c *converter) fill(out []byte, frames int, gain float64, pull func([]int16)) {
	if cap(c.mix) < frames*2 {
		c.mix = make([]float64, frames*2)
	}
	mix := c.mix[:frames*2]
	if c.resampler == nil {
		// One source block per output block; sizes match at 44.1 kHz.
		if cap(c.source) < frames*2 {
			c.source = make([]int16, frames*2)
		}
		src := c.source[:frames*2]
		clear(src)
		pull(src)
		for i, v := range src {
			mix[i] = float64(v) / 32768
		}
	} else {
		c.resampler.read(mix, func() []int16 {
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
	dither := c.dither == "on" || c.dither == "auto" && c.bits == 16 && modified
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

// resampler converts interleaved stereo with a windowed-sinc kernel. Only
// upsampling from 44.1 kHz is configured, so the cutoff is below the source
// Nyquist frequency.
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
	cutoff := 0.5 * 0.91 // of the source rate, leaving a transition band below 22.05 kHz
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

func (r *resampler) read(out []float64, more func() []int16) {
	for i := 0; i+1 < len(out); i += 2 {
		for !r.ready() {
			for _, v := range more() {
				r.hist = append(r.hist, float64(v)/32768)
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

// StreamResampler converts interleaved int16 stereo from one rate to another
// as blocks arrive, keeping its state between blocks so they join without
// clicks. The output of a block can be a frame shorter or longer than the
// exact ratio; the total over many blocks follows it.
type StreamResampler struct{ r *resampler }

func NewStreamResampler(from, to int) *StreamResampler {
	return &StreamResampler{r: newResampler(from, to)}
}

// Process takes one block and returns the frames that can be produced so far.
func (s *StreamResampler) Process(in []int16) []int16 {
	r := s.r
	for _, v := range in {
		r.hist = append(r.hist, float64(v)/32768)
	}
	var out []int16
	for r.ready() {
		left, right := r.next()
		out = append(out, toInt16(left), toInt16(right))
	}
	r.trim()
	return out
}

func toInt16(v float64) int16 {
	v = math.Round(v * 32768)
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}
