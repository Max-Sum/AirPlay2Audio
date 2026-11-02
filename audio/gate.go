package audio

import (
	"log"
	"sync/atomic"
	"time"

	"goplay2/globals"
)

// Several senders may be connected at once, but they share one Player.
// Only the active stream uses it; a stream on standby keeps its sockets
// open but does not queue audio, move the clock or start and stop output.
// A buffered (TCP) stream on standby stops reading, so the sender is
// back-pressured instead of losing the audio it already sent.

type streamGate struct {
	standby atomic.Bool
	wake    chan struct{} // closed when the stream becomes active again
	// FLUSHBUFFERED requests received on standby, applied when the stream
	// resumes.
	flushes     []flushRequest
	realtimeCap int  // ring capacity used by a UDP stream in low latency mode; 0 for TCP
	classic     bool // AirPlay 1: the clock follows the sender over NTP
	// What the monitor shows for this stream, restored when it resumes.
	kind, codec string
	latency     time.Duration
	rate, bits  int
	// The latest timing anchor of a realtime or AirPlay 1 stream, applied
	// at once when it resumes instead of waiting for the next one.
	anchorNet   int64
	anchorStamp uint32
	haveAnchor  bool
}

// A realtime sender that was paused while another sender plays keeps its
// stream open for a while and goes on sending packets of silence. When its
// user presses play again it sends no SETUP, and not always a progress
// update (a live stream has none); its packets simply carry audio again.
// standbyWatch notices that: first a pause (sustained digital silence),
// then sustained sound. A sender that ignored the pause and kept playing
// never goes silent, so it does not take the output back.
const (
	standbyPausedAfter  = 500 * time.Millisecond
	standbyResumedAfter = 150 * time.Millisecond
)

type standbyWatch struct {
	silent, sound int64 // consecutive samples
	paused          bool
}

func (w *standbyWatch) reset() { *w = standbyWatch{} }

func (w *standbyWatch) observe(s *Server, frame *PCMFrame) {
	n := int64(len(frame.pcmData) / 4)
	if frame.silent() {
		w.silent += n
		w.sound = 0
		if !w.paused && w.silent >= int64(standbyPausedAfter)*sampleRate/int64(time.Second) {
			w.paused = true
			log.Print("AUDIO realtime waiting sender is silent (paused)")
		}
		return
	}
	w.sound += n
	w.silent = 0
	if w.paused && w.sound >= int64(standbyResumedAfter)*sampleRate/int64(time.Second) {
		w.reset()
		log.Print("AUDIO realtime waiting sender is sending audio again")
		if resume := s.OnResume; resume != nil {
			go resume()
		}
	}
}

// silent reports digital silence (allowing for dither).
func (f *PCMFrame) silent() bool {
	for i := 0; i+1 < len(f.pcmData); i += 2 {
		v := int16(uint16(f.pcmData[i]) | uint16(f.pcmData[i+1])<<8)
		if v > 4 || v < -4 {
			return false
		}
	}
	return true
}

// describe records the stream for the monitor panel.
func (s *Server) describe(kind, codec string) {
	s.mu.Lock()
	s.gate.kind, s.gate.codec, s.gate.latency = kind, codec, 0
	s.gate.rate, s.gate.bits = 0, 0
	s.mu.Unlock()
	if s.Active() {
		s.player.setStream(kind, codec)
	}
}

// noteFormat records the sample rate and sample size the sender encodes at.
func (s *Server) noteFormat(rate, bits int) {
	s.mu.Lock()
	s.gate.rate, s.gate.bits = rate, bits
	s.mu.Unlock()
	if s.Active() {
		s.player.setFormat(rate, bits)
	}
}

// addBytes counts compressed audio received, for the monitor's bit rate.
func (s *Server) addBytes(n int) {
	if n > 0 && s.Active() {
		s.player.audioBytes.Add(uint64(n))
	}
}

func (s *Server) noteLatency(d time.Duration) {
	s.mu.Lock()
	s.gate.latency = d
	s.mu.Unlock()
	if s.Active() {
		s.player.setLatency(d)
	}
}

// anchor passes a timing anchor to the player while the stream is active,
// and keeps it for when a stream on standby resumes.
func (s *Server) anchor(networkTime int64, stamp uint32) {
	s.mu.Lock()
	s.gate.anchorNet, s.gate.anchorStamp, s.gate.haveAnchor = networkTime, stamp, true
	s.mu.Unlock()
	if s.Active() {
		s.player.ControlChannel <- globals.ControlMessage{MType: globals.ANCHOR, Param1: networkTime, Param2: int64(stamp)}
	}
}

// Active reports whether the stream may use the player.
func (s *Server) Active() bool { return !s.gate.standby.Load() }

// Standby marks a new stream as inactive without touching the player,
// which another sender is using.
func (s *Server) Standby() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gate.standby.Load() {
		return
	}
	s.gate.standby.Store(true)
	s.gate.wake = make(chan struct{})
	s.pauseTiming(true)
}

// Suspend takes the player away from this stream: queued audio is dropped
// and output pauses, so another stream can start.
func (s *Server) Suspend() {
	s.mu.Lock()
	if s.gate.standby.Load() {
		s.mu.Unlock()
		return
	}
	s.gate.standby.Store(true)
	s.gate.wake = make(chan struct{})
	// Packets decoded before this point must not reach the player.
	s.flushEpoch.Add(1)
	s.pauseTiming(true)
	s.player.Reset()
	s.mu.Unlock()
	s.player.ControlChannel <- globals.ControlMessage{MType: globals.PAUSE}
}

// Resume gives the player back to this stream, configured as it was set up.
func (s *Server) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.gate.standby.Load() {
		return
	}
	s.player.Reset()
	if !s.player.synchronized {
		if s.gate.realtimeCap > 0 {
			s.player.ringBuffer.ResetCapacity(s.gate.realtimeCap)
			s.player.SetLowLatencyCatchUp(true)
		} else {
			s.player.ringBuffer.ResetCapacity(s.player.baseCapacity)
			s.player.SetLowLatencyCatchUp(false)
		}
	}
	if s.gate.classic {
		// The clock followed another sender meanwhile.
		s.player.clock.virtualClock.Reset()
	}
	for _, f := range s.gate.flushes {
		if f.immediate {
			s.player.flushImmediate(uint64(f.until))
		} else {
			s.player.skipUntil(int64(f.from), int64(f.until))
		}
	}
	s.gate.flushes = nil
	s.restart.Add(1)
	if s.gate.kind != "" {
		s.player.setStream(s.gate.kind, s.gate.codec)
		if s.gate.latency != 0 {
			s.player.setLatency(s.gate.latency)
		}
		s.player.setFormat(s.gate.rate, s.gate.bits)
	} else {
		s.player.clearStream()
	}
	s.flushEpoch.Add(1)
	s.gate.standby.Store(false)
	close(s.gate.wake)
	s.gate.wake = nil
	s.pauseTiming(false)
	if s.gate.haveAnchor && !s.gate.classic {
		// The player still has the previous sender's anchor. (AirPlay 1
		// restarts NTP timing, so its next sync packet anchors it.)
		s.player.ControlChannel <- globals.ControlMessage{MType: globals.ANCHOR, Param1: s.gate.anchorNet, Param2: int64(s.gate.anchorStamp)}
	} else if s.gate.kind == "realtime" {
		// This stream's anchor was reset (no control packets for a while):
		// do not play its audio on the previous sender's anchor.
		s.player.ControlChannel <- globals.ControlMessage{MType: globals.UNANCHOR}
	}
}

// waitActive blocks a TCP reader until the stream is active or done closes.
func (s *Server) waitActive(done <-chan struct{}) bool {
	s.mu.Lock()
	wake := s.gate.wake
	s.mu.Unlock()
	if wake == nil {
		return true
	}
	select {
	case <-wake:
		return true
	case <-done:
		return false
	}
}

// pauseTiming stops an AirPlay 1 NTP client from moving the shared clock.
// Called with s.mu held.
func (s *Server) pauseTiming(paused bool) {
	if t, ok := s.timing.(interface{ SetPaused(bool) }); ok {
		t.SetPaused(paused)
	}
}
