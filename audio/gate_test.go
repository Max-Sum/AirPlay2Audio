package audio

import (
	"testing"
	"time"

	"goplay2/globals"
	"goplay2/ptp"
)

func TestStandbyStreamLeavesPlayerAlone(t *testing.T) {
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(16))
	s, _ := NewServer(player)
	s.Standby()
	if s.Active() {
		t.Fatal("stream active after Standby")
	}
	s.SetRateAnchorTime(1, time.Now())
	s.SetRate0()
	s.Flush(10, 20)
	s.Teardown()
	if n := len(player.ControlChannel); n != 0 {
		t.Fatalf("standby stream sent %d player messages", n)
	}
	player.flushMu.Lock()
	flushing := len(player.flushes)
	player.flushMu.Unlock()
	if flushing != 0 {
		t.Fatal("standby flush applied to the shared player")
	}

	// The flush is applied once the stream resumes.
	s.Resume()
	if !s.Active() {
		t.Fatal("stream not active after Resume")
	}
	player.flushMu.Lock()
	flushes := append([]flushRequest(nil), player.flushes...)
	player.flushMu.Unlock()
	if len(flushes) != 1 || flushes[0] != (flushRequest{from: 10, until: 20}) {
		t.Fatalf("pending flush not applied: %+v", flushes)
	}
}

func TestStandbyRealtimeStreamDoesNotClearActiveAnchor(t *testing.T) {
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(16))
	s, _ := NewServer(player)
	s.Standby()
	s.anchor(1, 2)
	if !s.forgetAnchor() {
		t.Fatal("anchor not reported as forgotten")
	}
	if n := len(player.ControlChannel); n != 0 {
		t.Fatalf("standby stream sent %d player messages", n)
	}
	active, _ := NewServer(player)
	active.anchor(1, 2)
	<-player.ControlChannel // ANCHOR
	active.forgetAnchor()
	if msg := <-player.ControlChannel; msg.MType != globals.UNANCHOR {
		t.Fatalf("active stream sent %v, want UNANCHOR", msg.MType)
	}
}

func TestLeftoverControlPacket(t *testing.T) {
	packet := make([]byte, 28)
	packet[0], packet[1] = 0x80, 0xd7
	if !leftoverControlPacket(packet, time.Millisecond) {
		t.Fatal("non-sentinel packet at start not dropped")
	}
	packet[0] = 0x90
	if leftoverControlPacket(packet, time.Millisecond) {
		t.Fatal("sentinel packet dropped")
	}
	packet[0] = 0x80
	if leftoverControlPacket(packet, 10*time.Millisecond) {
		t.Fatal("packet after the start window dropped")
	}
}

func TestStandbyWatchNeedsPauseThenSound(t *testing.T) {
	silence := testPCM(0, 0, make([]int16, 352*2)...)
	loud := make([]int16, 352*2)
	for i := range loud {
		loud[i] = 3000
	}
	sound := testPCM(0, 0, loud...)
	feed := func(w *standbyWatch, s *Server, frame *PCMFrame, d time.Duration) {
		for i := int64(0); i < int64(d)*sampleRate/int64(time.Second)/352+1; i++ {
			w.observe(s, frame)
		}
	}
	calls := make(chan struct{}, 4)
	s := &Server{OnResume: func() { calls <- struct{}{} }}
	count := func() int {
		time.Sleep(20 * time.Millisecond) // OnResume runs on its own goroutine
		return len(calls)
	}

	// Still playing (ignored the pause): sound only, never a resume.
	var w standbyWatch
	feed(&w, s, sound, 2*time.Second)
	if count() != 0 {
		t.Fatal("a sender that never paused took the output back")
	}
	// Short gaps in the music are not a pause.
	feed(&w, s, silence, 200*time.Millisecond)
	feed(&w, s, sound, 300*time.Millisecond)
	if count() != 0 {
		t.Fatal("a short silence counted as a pause")
	}
	// Paused, then playing again.
	feed(&w, s, silence, 600*time.Millisecond)
	feed(&w, s, sound, 100*time.Millisecond)
	if count() != 0 {
		t.Fatal("resumed before the sound was sustained")
	}
	feed(&w, s, sound, 100*time.Millisecond)
	if count() != 1 {
		t.Fatalf("resume reported %d times, want 1", count())
	}
	feed(&w, s, sound, time.Second)
	if count() != 1 {
		t.Fatal("resume reported again without a new pause")
	}
}

func TestSuspendPausesAndBlocksPush(t *testing.T) {
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(16))
	s, _ := NewServer(player)
	s.Suspend()
	if msg := <-player.ControlChannel; msg.MType != globals.PAUSE {
		t.Fatalf("suspend sent %v, want PAUSE", msg.MType)
	}
	done := make(chan struct{})
	frame := &PCMFrame{}
	pushed := make(chan bool, 1)
	go func() {
		if s.waitActive(done) {
			pushed <- player.pushIf(frame, done, s.Active)
		}
	}()
	select {
	case <-pushed:
		t.Fatal("suspended stream pushed audio")
	case <-time.After(50 * time.Millisecond):
	}
	s.Resume()
	select {
	case ok := <-pushed:
		if !ok || player.ringBuffer.Length() != 1 {
			t.Fatal("resumed stream did not push the held frame")
		}
	case <-time.After(time.Second):
		t.Fatal("resumed stream still blocked")
	}
	close(done)
}
