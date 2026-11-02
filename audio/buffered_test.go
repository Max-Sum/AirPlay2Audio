package audio

import (
	"math"
	"sync/atomic"
	"testing"

	"goplay2/codec"
)

func TestRealtimeAnchorModes(t *testing.T) {
	frame1, frame2 := uint32(100000), uint32(100000+77175)
	if got := realtimeAnchorStamp(frame1, frame2, AnchorShairport); got != frame1-11025 {
		t.Fatalf("shairport mode = %d, want frame_1 - 11025", got)
	}
	if got := realtimeAnchorStamp(frame1, frame2, AnchorLowLatency); got != frame2-11025 {
		t.Fatalf("low-latency mode = %d, want frame_2 - 11025", got)
	}
	// An implausible latency is ignored in low-latency mode.
	if got := realtimeAnchorStamp(frame1, frame1-5, AnchorLowLatency); got != frame1-11025 {
		t.Fatalf("negative latency = %d", got)
	}
	if _, err := ParseRealtimeAnchor("video"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestRTPMapperIdentityAt44100(t *testing.T) {
	m := newRTPMapper(44100)
	for _, ts := range []uint32{0, 12345, math.MaxUint32} {
		if got := m.Map(ts); got != ts {
			t.Fatalf("Map(%d) = %d", ts, got)
		}
	}
}

// At 48 kHz the mapping scales by 44100/48000 and stays continuous across
// the 32-bit wrap of the source timestamps.
func TestRTPMapperScalesAcrossWrap(t *testing.T) {
	m := newRTPMapper(48000)
	start := uint32(math.MaxUint32 - 48000/2) // wraps after half a second
	first := m.Map(start)
	previous := first
	for i := 1; i <= 40; i++ {
		got := m.Map(start + uint32(i)*1024)
		step := int32(got - previous)
		if step < 940 || step > 941 {
			t.Fatalf("block %d advanced %d samples, want 940 or 941", i, step)
		}
		previous = got
	}
	if total := int32(previous - first); total != 40*1024*44100/48000 {
		t.Fatalf("40 blocks advanced %d samples", total)
	}
	// An anchor before the packets maps on the same timeline.
	if got := int32(m.Map(start) - first); got != 0 {
		t.Fatalf("anchor mapped %d samples away", got)
	}
}

func TestStreamResamplerKeepsRatio(t *testing.T) {
	r := codec.NewStreamResampler(48000, 44100)
	in := make([]int16, 1024*2)
	total := 0
	for i := 0; i < 100; i++ {
		total += len(r.Process(in)) / 2
	}
	want := 100 * 1024 * 44100 / 48000
	if total < want-32 || total > want+1 {
		t.Fatalf("produced %d frames, want about %d", total, want)
	}
}

func TestToStereo16Rounds24Bit(t *testing.T) {
	put := func(v int32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16)} }
	pcm := append(append(append(put(0x7fffff), put(-0x800000)...), put(0x000180)...), put(-0x000100)...)
	got := toStereo16(pcm, formatALAC48000S24)
	want := []int16{32767, -32768, 2, -1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestSurroundFormatsAreAdvertisedAndConfigured(t *testing.T) {
	for _, format := range []uint64{1 << 39, 1 << 40, 1 << 21, 1 << 22, 1 << 23} {
		if !SupportedBufferedFormat(format) {
			t.Fatalf("format %#x not supported", format)
		}
	}
	if SupportedBufferedFormat(1<<22|1<<23) || SupportedBufferedFormat(1<<24) {
		t.Fatal("a combined or unknown format bit accepted")
	}
	if formatForAudioFormat(1<<39) != formatAAC48000x51 || formatForAudioFormat(1<<40) != formatAAC48000x71 {
		t.Fatal("surround SETUP formats")
	}
	// AudioSpecificConfig channel configuration: 6 for 5.1, 7 for 7.1.
	if c := aacConfig(formatAAC48000x51); c[1]>>3&0xf != 6 {
		t.Fatalf("5.1 config %x", c)
	}
	if c := aacConfig(formatAAC48000x71); c[1]>>3&0xf != 7 {
		t.Fatalf("7.1 config %x", c)
	}
}

func TestAACMutesFirstBlockAndGaps(t *testing.T) {
	var restart atomic.Uint64
	d := &bufferedDecoder{restart: &restart}
	steps := []struct {
		ts   uint32
		mute bool
	}{{1000, true}, {2024, false}, {3048, false}, {5000, true}, {6024, false}}
	for i, s := range steps {
		if got := d.mute(s.ts, 1024); got != s.mute {
			t.Fatalf("step %d: mute=%t, want %t", i, got, s.mute)
		}
	}
	restart.Add(1) // pause, flush or resume
	if !d.mute(7048, 1024) {
		t.Fatal("first block after a restart not muted")
	}
}

func TestBufferedFormatSelection(t *testing.T) {
	d := newBufferedDecoder(formatAAC44100, nil)
	if f := d.formatFor(ssrcAAC48000); f != formatAAC48000 {
		t.Fatalf("SSRC %#x gave %s", ssrcAAC48000, f.name)
	}
	if f := d.formatFor(ssrcALAC48000S24); f != formatALAC48000S24 {
		t.Fatalf("ALAC SSRC gave %s", f.name)
	}
	if f := d.formatFor(0x12345678); f != formatAAC44100 {
		t.Fatalf("unknown SSRC gave %s, want the SETUP format", f.name)
	}
	if formatForAudioFormat(1<<23) != formatAAC48000 || formatForAudioFormat(0) != formatAAC44100 {
		t.Fatal("SETUP audioFormat mapping")
	}
}
