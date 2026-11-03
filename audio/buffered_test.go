package audio

import (
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/Max-Sum/AirPlay2Audio/codec"
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

// 24-bit lossless audio keeps all of its bits.
func TestPCM24KeepsEveryBit(t *testing.T) {
	put := func(v int32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16)} }
	want := []int32{0x7fffff, -0x800000, 0x000180, -1}
	var pcm []byte
	for _, v := range want {
		pcm = append(pcm, put(v)...)
	}
	got := pcm24(pcm)
	for i := range want {
		if got[i]>>8 != want[i] || got[i]&0xff != 0 {
			t.Fatalf("sample %d = %#x, want %#x", i, got[i], want[i]<<8)
		}
	}
	if got := pcm16([]byte{0x01, 0x80, 0xff, 0x7f}); got[0] != -32767<<16 || got[1] != 32767<<16 {
		t.Fatalf("16-bit samples %v", got)
	}
}

// Decoded audio keeps the stream's own rate and sample size.
func TestBufferedFramesKeepTheStreamFormat(t *testing.T) {
	cases := []struct {
		format bufferedFormat
		want   codec.Format
	}{
		{formatAAC44100, codec.Format{Rate: 44100, Bits: 16}},
		{formatAAC48000, codec.Format{Rate: 48000, Bits: 16}},
		{formatALAC48000S24, codec.Format{Rate: 48000, Bits: 24}},
		{formatAAC48000x51, codec.Format{Rate: 48000, Bits: 16}},
	}
	for _, c := range cases {
		if got := c.format.pcmFormat(); got != c.want {
			t.Errorf("%s decodes to %v, want %v", c.format.name, got, c.want)
		}
	}
}

// A frame in a new format waits until the output asks for the format; the
// player then plays it, reading the anchor at the new rate.
func TestPlayerSwitchesFormatAtFrameBoundary(t *testing.T) {
	cd := codec.Format{Rate: 44100, Bits: 16}
	hi := codec.Format{Rate: 48000, Bits: 24}
	p := &Player{ringBuffer: NewRing(4), format: cd, clock: &Clock{}}
	p.SetSynchronized(false)
	p.Push(formatPCM(cd, 0, 1, 1, 1, 2, 2))
	p.Push(formatPCM(hi, 0, 2, 3, 3, 4, 4))
	out := make([]int32, 8)
	p.Fill(out, 0, 0)
	if want := []int32{1, 1, 2, 2, 0, 0, 0, 0}; !reflect.DeepEqual(out, want) {
		t.Fatalf("first block %v, want the 44.1 kHz frame then silence", out)
	}
	if f := p.Format(); f != hi {
		t.Fatalf("format %v after a 48 kHz frame, want %v", f, hi)
	}
	if p.clock.rate != 48000 {
		t.Fatalf("clock counts at %d Hz", p.clock.rate)
	}
	p.Fill(out, 0, 0)
	if want := []int32{3, 3, 4, 4, 0, 0, 0, 0}; !reflect.DeepEqual(out, want) {
		t.Fatalf("second block %v", out)
	}
	if f := p.Format(); f != hi {
		t.Fatalf("format changed again to %v", f)
	}
}

func TestSurroundFormatsAreAdvertisedAndConfigured(t *testing.T) {
	for _, format := range []uint64{1 << 39, 1 << 21, 1 << 22, 1 << 23} {
		if !SupportedBufferedFormat(format) {
			t.Fatalf("format %#x not supported", format)
		}
	}
	if SupportedBufferedFormat(1<<40) != decodes71 {
		t.Fatalf("7.1 advertised: %v, decodable: %v", !decodes71, decodes71)
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
	d := newBufferedDecoder(formatAAC44100)
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
