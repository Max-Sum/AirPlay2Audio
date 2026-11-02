package audio

import (
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/Max-Sum/AirPlay2Audio/codec"
	"github.com/Max-Sum/AirPlay2Audio/globals"
)

type testStream struct{ calls chan string }

func (s *testStream) Init(codec.StreamCallback) error { return nil }
func (s *testStream) Start() error                    { s.calls <- "start"; return nil }
func (s *testStream) Stop() error                     { s.calls <- "stop"; return nil }
func (s *testStream) Close() error                    { return nil }
func (s *testStream) SetVolume(float64) error         { return nil }

func testPCM(timestamp uint32, seq uint32, samples ...int16) *PCMFrame {
	data := make([]byte, len(samples)*2)
	for i, value := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(value))
	}
	return &PCMFrame{TcpPacket: TcpPacket{Packet: rtp.Packet{Header: rtp.Header{Timestamp: timestamp}}, SequenceNumber: seq}, pcmData: data}
}

func TestRenderAtAlignsSamplesAndWrapsRTP(t *testing.T) {
	cases := []struct {
		name   string
		frame  uint32
		target uint32
		want   []int16
	}{
		{"on time", 100, 100, []int16{1, 2, 3, 4, 5, 6, 0, 0}},
		{"early", 102, 100, []int16{0, 0, 0, 0, 1, 2, 3, 4}},
		{"late", 100, 101, []int16{3, 4, 5, 6, 0, 0, 0, 0}},
		{"wrap", 0, ^uint32(0), []int16{0, 0, 1, 2, 3, 4, 5, 6}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Player{ringBuffer: NewRing(3)}
			p.Push(testPCM(tc.frame, 1, 1, 2, 3, 4, 5, 6))
			out := make([]int16, 8)
			p.renderAt(out, tc.target)
			if !reflect.DeepEqual(out, tc.want) {
				t.Fatalf("got %v, want %v", out, tc.want)
			}
		})
	}
}

func TestRenderAtRetainsPartiallyPlayedFrame(t *testing.T) {
	p := &Player{ringBuffer: NewRing(3)}
	p.Push(testPCM(10, 1, 1, 2, 3, 4, 5, 6))
	first, second := make([]int16, 4), make([]int16, 4)
	p.renderAt(first, 10)
	p.renderAt(second, 12)
	if !reflect.DeepEqual(first, []int16{1, 2, 3, 4}) || !reflect.DeepEqual(second, []int16{5, 6, 0, 0}) {
		t.Fatalf("unexpected successive output: %v / %v", first, second)
	}
}

func TestUnsynchronizedPlayoutDoesNotWaitForClock(t *testing.T) {
	p := &Player{ringBuffer: NewRing(3)}
	p.Push(testPCM(100000, 1, 1, 2, 3, 4, 5, 6))
	p.Push(testPCM(200000, 2, 7, 8))
	first, second := make([]int16, 4), make([]int16, 4)
	p.callBack(first, 0, 0)
	p.callBack(second, 0, 0)
	if !reflect.DeepEqual(first, []int16{1, 2, 3, 4}) || !reflect.DeepEqual(second, []int16{5, 6, 7, 8}) {
		t.Fatalf("unexpected unsynchronized output: %v / %v", first, second)
	}
}

// Feed one 352-sample packet per 352 output samples on top of an initial
// backlog, as a realtime sender does, and return the final queued samples.
func runCatchUp(t *testing.T, enabled bool, initial int) int {
	t.Helper()
	p := &Player{ringBuffer: NewRing(256)}
	p.SetSynchronized(false)
	p.SetLowLatencyCatchUp(enabled)
	packet := make([]int16, 352*2)
	for i := range packet {
		packet[i] = int16(i)
	}
	seq := uint32(0)
	push := func() { seq++; p.Push(testPCM(seq*352, seq, packet...)) }
	for i := 0; i < initial; i++ {
		push()
	}
	out := make([]int16, 352*2)
	for i := 0; i < 44100*40/352; i++ {
		push()
		p.callBack(out, 0, 0)
	}
	return p.backlog()
}

func TestLowLatencyCatchUpDrainsPersistentBacklog(t *testing.T) {
	// 50 packets is about 400 ms of delay left behind by earlier stalls.
	if got := runCatchUp(t, false, 50); got < 50*352 {
		t.Fatalf("without catch-up backlog = %d, expected it to stay", got)
	}
	got := runCatchUp(t, true, 50)
	if got > catchUpTarget+2*352 {
		t.Fatalf("catch-up left %d samples queued, want about %d", got, catchUpTarget)
	}
	// A backlog already within the jitter margin is left alone.
	if got := runCatchUp(t, true, 2); got < 2*352 {
		t.Fatalf("small backlog was reduced to %d", got)
	}
}

func TestCompressKeepsFrameEndpoints(t *testing.T) {
	p := &Player{dropBudget: 1000}
	samples := make([]int16, 100*2)
	for i := range samples {
		samples[i] = int16(i * 10)
	}
	out := p.compress(testPCM(0, 1, samples...))
	n := len(out.pcmData) / 4
	if n != 98 || p.dropBudget != 998 {
		t.Fatalf("compressed to %d samples, budget %d", n, p.dropBudget)
	}
	first := int16(binary.LittleEndian.Uint16(out.pcmData[0:]))
	last := int16(binary.LittleEndian.Uint16(out.pcmData[(n*2-1)*2:]))
	if first != samples[0] || last != samples[len(samples)-1] {
		t.Fatalf("endpoints changed: %d, %d", first, last)
	}
}

func TestCancelledProducerCannotEnterReplacementSession(t *testing.T) {
	r := NewRing(1)
	r.Push(testPCM(0, 1, 1, 1))
	done := make(chan struct{})
	result := make(chan bool, 1)
	go func() { result <- r.PushUntil(testPCM(1, 2, 2, 2), done) }()
	close(done)
	r.Reset()
	select {
	case pushed := <-result:
		if pushed || !r.IsEmpty() {
			t.Fatal("old session inserted a frame after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not wake after reset")
	}
}

func TestUnsynchronizedFlushDiscardsOnlyRequestedFrames(t *testing.T) {
	p := &Player{ringBuffer: NewRing(4)}
	p.Push(testPCM(10, 10, 1, 1, 2, 2))
	p.Push(testPCM(12, 11, 3, 3))
	p.Push(testPCM(13, 12, 4, 4))
	first := make([]int16, 2)
	p.callBack(first, 0, 0)
	p.skipUntil(10, 12) // flushUntilSeq is the first frame kept
	second := make([]int16, 2)
	p.callBack(second, 0, 0)
	if !reflect.DeepEqual(first, []int16{1, 1}) || !reflect.DeepEqual(second, []int16{4, 4}) {
		t.Fatalf("flush produced %v / %v", first, second)
	}
}

func TestFlushFiltersBlockedAndFutureFramesAcrossWrap(t *testing.T) {
	p := &Player{ringBuffer: NewRing(1)}
	done := make(chan struct{})
	p.Push(testPCM(0, 0x7ffffe, 1, 1))
	result := make(chan bool, 1)
	go func() { result <- p.PushUntil(testPCM(1, 0x7fffff, 2, 2), done) }()
	// Removing the full ring wakes the producer; the frame it was holding
	// must be checked again against the newly installed flush interval.
	p.skipUntil(0x7ffffe, 1)
	select {
	case ok := <-result:
		if !ok || !p.ringBuffer.IsEmpty() {
			t.Fatal("flushed frame entered queue")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked producer was not woken")
	}
	if !p.PushUntil(testPCM(2, 0, 3, 3), done) || !p.ringBuffer.IsEmpty() {
		t.Fatal("later arriving frame in flushed range entered queue")
	}
	if !p.PushUntil(testPCM(3, 1, 4, 4), done) || p.ringBuffer.IsEmpty() {
		t.Fatal("first frame after flushed range (flushUntilSeq) was lost")
	}
	p.Reset()
	if !p.PushUntil(testPCM(4, 0x7fffff, 5, 5), done) || p.ringBuffer.IsEmpty() {
		t.Fatal("replacement session inherited flush filter")
	}
}

func TestImmediateFlushDropsQueueAndEarlierFrames(t *testing.T) {
	p := &Player{ringBuffer: NewRing(4)}
	done := make(chan struct{})
	p.Push(testPCM(0, 100, 1, 1))
	p.Push(testPCM(1, 101, 2, 2))
	p.flushImmediate(105)
	if !p.ringBuffer.IsEmpty() {
		t.Fatal("immediate flush kept queued audio")
	}
	for _, seq := range []uint32{102, 104} {
		if !p.PushUntil(testPCM(2, seq, 3, 3), done) || !p.ringBuffer.IsEmpty() {
			t.Fatalf("frame %d before flushUntilSeq entered queue", seq)
		}
	}
	if !p.PushUntil(testPCM(5, 105, 4, 4), done) || p.ringBuffer.Length() != 1 {
		t.Fatal("frame at flushUntilSeq was dropped")
	}
}

func TestImmediateFlushCancelsDeferredFlushes(t *testing.T) {
	p := &Player{ringBuffer: NewRing(4)}
	done := make(chan struct{})
	p.skipUntil(200, 210)
	p.flushImmediate(105)
	if !p.PushUntil(testPCM(0, 105, 1, 1), done) || !p.PushUntil(testPCM(1, 200, 2, 2), done) || p.ringBuffer.Length() != 2 {
		t.Fatal("deferred flush survived an immediate flush")
	}
}

func TestFirstAACFrameAfterFlushIsMuted(t *testing.T) {
	p := &Player{ringBuffer: NewRing(4)}
	done := make(chan struct{})
	p.skipUntil(10, 12)
	for seq := uint32(10); seq < 14; seq++ {
		frame := testPCM(seq, seq, 1000, 1000)
		frame.aac = true
		p.PushUntil(frame, done)
	}
	first := p.ringBuffer.Pop().(*PCMFrame)
	second := p.ringBuffer.Pop().(*PCMFrame)
	if first.SequenceNumber != 12 || !first.silent() || second.silent() {
		t.Fatalf("muting after flush: first=%d silent=%t, second silent=%t", first.SequenceNumber, first.silent(), second.silent())
	}
}

func TestUnsynchronizedStartAndPause(t *testing.T) {
	stream := &testStream{calls: make(chan string, 2)}
	p := &Player{clock: &Clock{}, stream: stream, ringBuffer: NewRing(2), ControlChannel: make(chan globals.ControlMessage, 2)}
	go p.Run()
	p.ControlChannel <- globals.ControlMessage{MType: globals.START}
	p.ControlChannel <- globals.ControlMessage{MType: globals.PAUSE}
	for _, want := range []string{"start", "stop"} {
		select {
		case got := <-stream.calls:
			if got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
	close(p.ControlChannel)
}

func TestPlayoutIgnoresSmallClockJitter(t *testing.T) {
	p := &Player{ringBuffer: NewRing(3)}
	p.Push(testPCM(100, 1, 1, 2, 3, 4, 5, 6))
	first, second := make([]int16, 4), make([]int16, 4)
	p.renderAt(first, p.nextPlayout(100, 2))
	// A jittery clock estimate is one sample ahead, but the continuous device
	// timeline should play the next two samples without dropping either.
	p.renderAt(second, p.nextPlayout(103, 2))
	if !reflect.DeepEqual(first, []int16{1, 2, 3, 4}) || !reflect.DeepEqual(second, []int16{5, 6, 0, 0}) {
		t.Fatalf("clock jitter disrupted audio: %v / %v", first, second)
	}
}

func TestFlushIncludesOldestAndFullRing(t *testing.T) {
	r := NewRing(3)
	for i := 0; i < 3; i++ {
		r.Push(i)
	}
	if count := r.Flush(func(v interface{}) bool { return v.(int) != 1 }); count != 2 {
		t.Fatalf("kept %d items", count)
	}
	if r.Pop() != 0 || r.Pop() != 2 || !r.IsEmpty() {
		t.Fatal("flush lost or reordered frames")
	}
}

func TestFlushSequenceRangeWrapsAt23Bits(t *testing.T) {
	p := &Player{ringBuffer: NewRing(4)}
	for _, seq := range []uint32{0x7ffffe, 0x7fffff, 0, 1} {
		p.Push(testPCM(seq, seq, 1, 1))
	}
	p.skipUntil(0x7fffff, 1)
	if p.ringBuffer.Length() != 2 {
		t.Fatalf("remaining frames: %d", p.ringBuffer.Length())
	}
	a := p.ringBuffer.Pop().(*PCMFrame).SequenceNumber
	b := p.ringBuffer.Pop().(*PCMFrame).SequenceNumber
	if a != 0x7ffffe || b != 1 {
		t.Fatalf("unexpected surviving sequences %#x, %#x", a, b)
	}
}
