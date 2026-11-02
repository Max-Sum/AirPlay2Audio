package audio

import (
	"encoding/binary"
	"goplay2/codec"
	"goplay2/globals"
	"goplay2/ptp"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

type PlaybackStatus uint8

const (
	STOPPED PlaybackStatus = iota
	PLAYING
)

type Player struct {
	ControlChannel chan globals.ControlMessage
	clock          *Clock
	Status         PlaybackStatus
	stream         codec.Stream
	ringBuffer     *Ring
	baseCapacity   int
	mu             sync.Mutex
	frame          *PCMFrame
	frameOffset    int // stereo samples, not bytes
	reportedOutput bool
	playoutRTP     uint32
	havePlayout    bool
	clockOutliers  int
	synchronized   bool
	flushMu        sync.Mutex
	flushes        []flushRequest // FLUSHBUFFERED ranges still filtering input
	flushDropped   bool           // a frame was dropped; mute the next AAC frame
	// Low-latency catch-up (unsynchronized realtime only); see trackBacklog.
	catchUp      bool
	frameSamples int
	backlogMin   int
	windowFrames int
	dropBudget   int
	catchingUp   bool

	// Drift correction (synchronized mode); see planPlayout.
	sync      SyncOptions
	syncError float64 // smoothed sender-minus-playout error, in samples
	stuffing  int     // current correction: +1 consume extra, -1 consume fewer
	scratch   []int16

	stats statistics
	net   netCounters
	info  StreamInfo // guarded by mu
	// audioBytes counts the compressed audio received; Monitor turns it into
	// a bit rate. bytesAt is when it was last read (guarded by statsMu).
	audioBytes atomic.Uint64
	bytesAt    time.Time
	// statsMu serializes Monitor and Statistics; unlogged holds what Monitor
	// took since the last Statistics line.
	statsMu  sync.Mutex
	unlogged period

	// Active state, as shairport-sync's sessioncontrol: the device is kept
	// open for activeTimeout after playback stops, then released.
	activeTimeout time.Duration
	// volumeRange is the attenuation in dB at the lowest sender volume.
	volumeRange float64
	// realtimeAnchor selects how realtime anchors are interpreted.
	realtimeAnchor RealtimeAnchor
	// OnEvent, when set, is called (from the player goroutine) with
	// "active-begin", "play-begin", "play-end" and "active-end".
	OnEvent func(event string)
	// OnAnchorClock, when set, is called with the PTP clock identity a
	// realtime anchor refers to.
	OnAnchorClock func(id uint64)
}

// SyncOptions controls synchronized playout.
type SyncOptions struct {
	// DriftCorrection adds or removes one sample at a time, spread over an
	// output block, to keep playback locked to the sender's clock.
	DriftCorrection bool
	// Tolerance is the smoothed timing error allowed before correcting.
	Tolerance time.Duration
	// Resync is the error beyond which playback jumps instead of
	// correcting gradually.
	Resync time.Duration
}

var DefaultSyncOptions = SyncOptions{DriftCorrection: true, Tolerance: 2 * time.Millisecond, Resync: 50 * time.Millisecond}

// SetSyncOptions configures synchronized playout. Call before Run.
func (p *Player) SetSyncOptions(o SyncOptions) {
	p.mu.Lock()
	p.sync = o
	p.mu.Unlock()
}

// SetActiveTimeout sets how long the output device stays open after
// playback stops. Zero keeps it open. Call before Run.
func (p *Player) SetActiveTimeout(d time.Duration) { p.activeTimeout = d }

func (p *Player) resyncLimit() int64 {
	if p.sync.Resync <= 0 {
		return sampleRate / 20
	}
	return int64(p.sync.Resync) * sampleRate / int64(time.Second)
}

const (
	// Backlog kept as jitter margin; anything persistently above it is delay.
	catchUpTarget = int(sampleRate) * 30 / 1000
	// The backlog minimum is taken over this many output samples.
	catchUpWindow = int(sampleRate) * 2
	// Remove at most one sample in this many (about 2% faster playback).
	catchUpRatio = 50
)

func NewPlayer(clock *ptp.VirtualClock, ring *Ring) *Player {
	return &Player{clock: &Clock{virtualClock: clock}, ControlChannel: make(chan globals.ControlMessage, 100), stream: codec.NewStream(), ringBuffer: ring, baseCapacity: ring.Capacity(), synchronized: true, sync: DefaultSyncOptions, volumeRange: DefaultVolumeRange}
}

// SetSynchronized selects timestamp-based playout. Configure it before Init.
// When false, audio is played as it arrives without PTP scheduling, as in
// UxPlay's audio-only sync=false mode. This cannot eliminate delays introduced
// by the sender or guarantee synchronization with video or other receivers.
func (p *Player) SetSynchronized(enabled bool) { p.synchronized = enabled }

// callBack fills a block according to the PTP/RTP anchor, including silence
// before an early packet and dropping samples whose playback time has passed.
func (p *Player) callBack(out []int16, currentTime, outputBufferDacTime time.Duration) {
	clear(out)
	p.mu.Lock()
	defer p.mu.Unlock()
	frames := len(out) / 2
	p.stats.observeQueue(p.ringBuffer.Length())
	if !p.synchronized {
		if p.catchUp {
			p.trackBacklog(frames)
		}
		written := p.renderFIFO(out)
		p.stats.output(frames, written)
		if written > 0 && !p.reportedOutput {
			p.reportedOutput = true
			log.Print("AUDIO unsynchronized PCM output active")
		}
		return
	}
	target, ok := p.clock.TargetRTP(outputBufferDacTime - currentTime)
	if !ok {
		p.stats.output(frames, 0)
		return
	}
	start, consume := p.planPlayout(target, frames)
	written := p.renderStretched(out, start, consume)
	p.stats.output(frames, written)
	if written > 0 && !p.reportedOutput {
		p.reportedOutput = true
		log.Print("AUDIO timed PCM output active")
	}
}

// renderStretched plays consume source samples (one more or one fewer than
// the block) into out by linear interpolation, keeping the first and last
// samples so successive blocks join without a step. This is how drift is
// corrected without audible clicks.
func (p *Player) renderStretched(out []int16, start uint32, consume int) int {
	frames := len(out) / 2
	if consume == frames || frames < 2 || consume < 2 {
		return p.renderAt(out, start)
	}
	if cap(p.scratch) < consume*2 {
		p.scratch = make([]int16, consume*2)
	}
	src := p.scratch[:consume*2]
	clear(src)
	written := p.renderAt(src, start)
	scale := float64(consume-1) / float64(frames-1)
	for i := 0; i < frames; i++ {
		pos := float64(i) * scale
		j := int(pos)
		if j >= consume-1 {
			j = consume - 2
		}
		frac := pos - float64(j)
		for c := 0; c < 2; c++ {
			a, b := float64(src[2*j+c]), float64(src[2*(j+1)+c])
			out[2*i+c] = int16(math.Round(a + (b-a)*frac))
		}
	}
	if written > frames {
		written = frames
	}
	return written
}

// renderFIFO preserves the order and all samples of frames received so far,
// while deliberately ignoring their future PTP presentation timestamps.
// The audio device still determines playback speed; it cannot play faster
// than real time or make the sender transmit a packet earlier.
func (p *Player) renderFIFO(out []int16) int {
	written := 0
	for cursor := 0; cursor+1 < len(out); {
		if p.frame == nil {
			item, err := p.ringBuffer.TryPop()
			if err != nil {
				break
			}
			p.frame, p.frameOffset = item.(*PCMFrame), 0
			if n := len(p.frame.pcmData) / 4; n > 0 {
				p.frameSamples = n
			}
			if p.catchUp && p.dropBudget > 0 {
				p.frame = p.compress(p.frame)
			}
		}
		available := len(p.frame.pcmData)/4 - p.frameOffset
		if available <= 0 {
			p.frame = nil
			continue
		}
		count := available
		if count > (len(out)-cursor)/2 {
			count = (len(out) - cursor) / 2
		}
		for i := 0; i < count*2; i++ {
			out[cursor+i] = int16(binary.LittleEndian.Uint16(p.frame.pcmData[p.frameOffset*4+i*2:]))
		}
		cursor += count * 2
		written += count
		p.frameOffset += count
		if count == available {
			p.frame = nil
		}
	}
	return written
}

// SetLowLatencyCatchUp enables delay control for unsynchronized realtime
// streams. Without it, every network stall or sender/sound-card clock
// difference leaves queued audio behind permanently, so the delay only grows.
// Buffered TCP streams must not use it: their sender intentionally keeps the
// queue full, so a large backlog there is not added delay.
func (p *Player) SetLowLatencyCatchUp(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.catchUp = enabled
	p.resetCatchUp()
}

func (p *Player) resetCatchUp() {
	p.backlogMin, p.windowFrames, p.dropBudget, p.catchingUp = -1, 0, 0, false
}

// backlog estimates queued stereo samples not yet handed to the device.
func (p *Player) backlog() int {
	n := p.ringBuffer.Length() * p.frameSamples
	if p.frame != nil {
		n += len(p.frame.pcmData)/4 - p.frameOffset
	}
	return n
}

// trackBacklog records the lowest backlog seen in each window. The minimum,
// not the average, is what can be removed without causing an underrun; the
// part of it above catchUpTarget becomes the next window's drop budget.
func (p *Player) trackBacklog(frames int) {
	if b := p.backlog(); p.backlogMin < 0 || b < p.backlogMin {
		p.backlogMin = b
	}
	p.windowFrames += frames
	if p.windowFrames < catchUpWindow {
		return
	}
	excess := p.backlogMin - catchUpTarget
	if excess < 0 {
		excess = 0
	}
	p.dropBudget = excess
	if excess > 0 && !p.catchingUp {
		log.Printf("AUDIO low-latency catch-up: removing %d ms of queued delay", int64(excess)*1000/sampleRate)
	}
	p.catchingUp = excess > 0
	p.backlogMin, p.windowFrames = -1, 0
}

// compress shortens one frame by up to 1/catchUpRatio of its samples using
// linear interpolation. The first and last samples are kept, so adjacent
// frames still join smoothly.
func (p *Player) compress(f *PCMFrame) *PCMFrame {
	n := len(f.pcmData) / 4
	d := n / catchUpRatio
	if d > p.dropBudget {
		d = p.dropBudget
	}
	m := n - d
	if d <= 0 || m < 2 {
		return f
	}
	sample := func(index, channel int) float64 {
		return float64(int16(binary.LittleEndian.Uint16(f.pcmData[(index*2+channel)*2:])))
	}
	out := make([]byte, m*4)
	scale := float64(n-1) / float64(m-1)
	for i := 0; i < m; i++ {
		pos := float64(i) * scale
		j := int(pos)
		if j >= n-1 {
			j = n - 2
		}
		frac := pos - float64(j)
		for c := 0; c < 2; c++ {
			a, b := sample(j, c), sample(j+1, c)
			binary.LittleEndian.PutUint16(out[(i*2+c)*2:], uint16(int16(math.Round(a+(b-a)*frac))))
		}
	}
	p.dropBudget -= d
	p.stats.compressed += uint64(d)
	return &PCMFrame{TcpPacket: f.TcpPacket, pcmData: out}
}

func (p *Player) nextPlayout(target uint32, frames int) uint32 {
	start, _ := p.planPlayout(target, frames)
	return start
}

// Smoothing of the timing error: a noisy PTP estimate or a quantized waveOut
// position must not cause corrections, while real drift is only about 0.1 ms
// per second.
const syncErrorSmoothing = 64

// planPlayout returns the RTP timestamp at which this output block starts and
// how many source samples it consumes. Advancing by exactly the block size
// keeps jitter from dropping or duplicating audio on every callback. Drift
// between the sender's clock and the sound card is corrected by consuming
// one sample more or fewer per block while the smoothed error is outside the
// tolerance, until it is back near zero (shairport-sync's "basic"
// interpolation, with the sample spread over the block). Playback only jumps
// after a large, sustained error.
func (p *Player) planPlayout(target uint32, frames int) (uint32, int) {
	if !p.havePlayout {
		p.playoutRTP, p.havePlayout = target, true
		p.syncError, p.stuffing = 0, 0
	}
	difference := int64(int32(target - p.playoutRTP))
	if limit := p.resyncLimit(); difference > limit || difference < -limit {
		p.clockOutliers++
		if p.clockOutliers >= 12 {
			log.Printf("AUDIO clock re-aligned by %d ms", difference*1000/sampleRate)
			p.playoutRTP = target
			p.clockOutliers = 0
			p.syncError, p.stuffing = 0, 0
			p.stats.resyncs++
		}
	} else {
		p.clockOutliers = 0
		p.syncError += (float64(difference) - p.syncError) / syncErrorSmoothing
		p.stats.observeError(difference)
	}
	consume := frames
	if p.sync.DriftCorrection {
		tolerance := float64(int64(p.sync.Tolerance) * sampleRate / int64(time.Second))
		switch {
		case p.syncError > tolerance:
			p.stuffing = 1
		case p.syncError < -tolerance:
			p.stuffing = -1
		case p.syncError > -tolerance/4 && p.syncError < tolerance/4:
			p.stuffing = 0
		}
		consume += p.stuffing
		// The correction shows in the next measurement; apply it to the
		// smoothed error now so the filter delay does not cause overshoot.
		p.syncError -= float64(p.stuffing)
		// Consuming an extra sample removes one from the output timeline.
		if p.stuffing > 0 {
			p.stats.removed++
		} else if p.stuffing < 0 {
			p.stats.inserted++
		}
	}
	result := p.playoutRTP
	p.playoutRTP += uint32(consume)
	return result, consume
}

// renderAt is called with the player lock held, after the output block has
// been cleared. Keeping the sample alignment independent of wall time also
// lets the timing and RTP wraparound behavior be tested deterministically.
func (p *Player) renderAt(out []int16, target uint32) int {
	written := 0
	for cursor := 0; cursor+1 < len(out); {
		if p.frame == nil {
			item, err := p.ringBuffer.TryPop()
			if err != nil {
				return written
			}
			p.frame, p.frameOffset = item.(*PCMFrame), 0
			if n := len(p.frame.pcmData) / 4; n > 0 {
				p.frameSamples = n // for backlog
			}
		}
		available := len(p.frame.pcmData)/4 - p.frameOffset
		if available <= 0 {
			p.frame = nil
			continue
		}
		frameTime := p.frame.Timestamp + uint32(p.frameOffset)
		blockTime := target + uint32(cursor/2)
		difference := int64(int32(frameTime - blockTime))
		if difference > 0 {
			silence := int(difference)
			if silence > (len(out)-cursor)/2 {
				return written
			}
			cursor += silence * 2
			continue
		}
		if difference < 0 {
			skip := int(-difference)
			if skip > available {
				skip = available
			}
			p.stats.late += uint64(skip)
			p.frameOffset += skip
			if skip == available {
				p.frame = nil
			}
			continue
		}
		count := available
		if count > (len(out)-cursor)/2 {
			count = (len(out) - cursor) / 2
		}
		for i := 0; i < count*2; i++ {
			start := p.frameOffset*4 + i*2
			out[cursor+i] = int16(binary.LittleEndian.Uint16(p.frame.pcmData[start:]))
		}
		cursor += count * 2
		written += count
		p.frameOffset += count
		if count == available {
			p.frame = nil
		}
	}
	return written
}

// ArrivalLead reports how long before its scheduled playback a sample with
// this RTP timestamp arrived, and records it for the monitor. Only
// meaningful in synchronized mode.
func (p *Player) ArrivalLead(timestamp uint32) (time.Duration, bool) {
	if !p.synchronized {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	target, ok := p.clock.TargetRTP(0)
	if !ok {
		return 0, false
	}
	lead := time.Duration(int64(int32(timestamp-target))) * time.Second / time.Duration(sampleRate)
	p.stats.observeLead(lead)
	return lead, true
}

func (p *Player) Init() error { return p.stream.Init(p.callBack) }

func (p *Player) emit(event string) {
	log.Printf("PLAYER %s", event)
	if p.OnEvent != nil {
		p.OnEvent(event)
	}
}

func (p *Player) Run() {
	defer p.stream.Close()
	active := false
	var idle *time.Timer
	var idleC <-chan time.Time
	stopIdle := func() {
		if idle != nil {
			idle.Stop()
			idle, idleC = nil, nil
		}
	}
	defer stopIdle()
	started := func() {
		stopIdle()
		if !active {
			active = true
			p.emit("active-begin")
		}
		p.emit("play-begin")
	}
	for {
		var msg globals.ControlMessage
		select {
		case m, ok := <-p.ControlChannel:
			if !ok {
				return
			}
			msg = m
		case <-idleC:
			idle, idleC = nil, nil
			if p.Status == STOPPED && active {
				active = false
				if releaser, ok := p.stream.(interface{ Release() error }); ok {
					if err := releaser.Release(); err != nil {
						globals.ErrLog.Printf("release audio output: %v", err)
					} else {
						log.Print("AUDIO output device released while idle")
					}
				}
				p.emit("active-end")
			}
			continue
		}
		switch msg.MType {
		case globals.PAUSE:
			if p.Status == PLAYING {
				if err := p.stream.Stop(); err != nil {
					globals.ErrLog.Printf("pause audio: %v", err)
				}
				p.emit("play-end")
				if p.activeTimeout > 0 {
					stopIdle()
					idle = time.NewTimer(p.activeTimeout)
					idleC = idle.C
				}
			}
			p.Status = STOPPED
			p.mu.Lock()
			p.havePlayout, p.clockOutliers = false, 0
			p.mu.Unlock()
		case globals.START:
			p.mu.Lock()
			p.clock.AnchorTime(msg.Param1, msg.Param2)
			p.havePlayout, p.clockOutliers = false, 0
			lead, ok := p.clock.AnchorLead()
			if ok {
				p.info.Latency = lead
			}
			p.mu.Unlock()
			if ok {
				log.Printf("AUDIO anchor lead=%d ms (sender scheduled playback this far ahead) output-delay=%d ms",
					lead.Milliseconds(), p.clock.virtualClock.OutputDelay().Milliseconds())
			}
			if p.Status == STOPPED {
				if err := p.stream.Start(); err != nil {
					globals.ErrLog.Printf("start audio: %v", err)
					continue
				}
				p.Status = PLAYING
				started()
			}
		case globals.ANCHOR:
			p.mu.Lock()
			p.clock.AnchorTime(msg.Param1, msg.Param2)
			p.mu.Unlock()
			if p.Status == STOPPED {
				if err := p.stream.Start(); err != nil {
					globals.ErrLog.Printf("start audio: %v", err)
					continue
				}
				p.Status = PLAYING
				started()
			}
		case globals.SKIP:
			p.skipUntil(msg.Param1, msg.Param2)
		case globals.UNANCHOR:
			p.clearAnchor()
		case globals.VOLUME:
			if err := p.stream.SetVolume(VolumeToAttenuation(msg.Paramf, p.volumeRange)); err != nil {
				globals.ErrLog.Printf("set volume: %v", err)
			}
		case globals.LOCAL_VOLUME:
			if local, ok := p.stream.(interface{ SetLocalVolume(float64) error }); ok {
				if err := local.SetLocalVolume(msg.Paramf); err != nil {
					globals.ErrLog.Printf("set local volume: %v", err)
				}
			} else {
				globals.ErrLog.Print("local volume is not supported by this audio backend")
			}
		}
	}
}

// SeqMask keeps the 23 bits of a buffered packet's sequence number; the top
// bit of the RTP sequence field is the marker, which senders always set.
const SeqMask = 0x7fffff

// seqDiff is a-b in 23-bit modular arithmetic, so ranges work across the wrap.
func seqDiff(a, b uint32) int32 { return int32((a-b)<<9) >> 9 }

// flushRequest is a FLUSHBUFFERED still filtering incoming frames. An
// immediate flush drops everything before until; a deferred flush drops the
// frames from from up to, not including, until (shairport-sync semantics:
// the until frame is the first one played again).
type flushRequest struct {
	immediate   bool
	from, until uint32
}

func (f flushRequest) drops(seq uint32) bool {
	if f.immediate {
		return seqDiff(seq, f.until) < 0
	}
	return seqDiff(seq, f.from) >= 0 && seqDiff(seq, f.until) < 0
}

// At most this many deferred flushes wait at once, as shairport-sync.
const maxFlushRequests = 25

// skipUntil applies a deferred flush: queued frames in the range are removed
// now, frames still to arrive are dropped as they come.
func (p *Player) skipUntil(fromSeq, untilSeq int64) {
	request := flushRequest{from: uint32(fromSeq) & SeqMask, until: uint32(untilSeq) & SeqMask}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushMu.Lock()
	if len(p.flushes) < maxFlushRequests {
		p.flushes = append(p.flushes, request)
	} else {
		log.Print("AUDIO too many deferred flushes waiting; one ignored")
	}
	p.flushMu.Unlock()
	keep := func(frame *PCMFrame) bool { return !request.drops(frame.SequenceNumber & SeqMask) }
	if p.frame != nil && !keep(p.frame) {
		p.frame = nil
	}
	p.ringBuffer.Flush(func(value interface{}) bool { return keep(value.(*PCMFrame)) })
}

// flushImmediate drops all queued audio and every frame that arrives before
// untilSeq, and cancels deferred flushes. untilSeq 0 drops only what is
// queued (HomePods send it; shairport-sync treats it the same way).
func (p *Player) flushImmediate(untilSeq uint64) {
	p.Reset()
	p.flushMu.Lock()
	p.flushes = p.flushes[:0]
	if until := uint32(untilSeq) & SeqMask; until != 0 {
		p.flushes = append(p.flushes, flushRequest{immediate: true, until: until})
	}
	p.flushMu.Unlock()
}

func (p *Player) Push(frame interface{}) { p.ringBuffer.Push(frame) }
func (p *Player) PushUntil(frame interface{}, done <-chan struct{}) bool {
	return p.ringBuffer.PushUntil(frame, done, p.acceptFrame)
}

// pushIf is PushUntil with an extra condition, checked again whenever a
// producer waiting for room wakes up.
func (p *Player) pushIf(frame interface{}, done <-chan struct{}, allowed func() bool) bool {
	return p.ringBuffer.PushUntil(frame, done, func(value interface{}) bool {
		return allowed() && p.acceptFrame(value)
	})
}

// acceptFrame drops frames inside a FLUSHBUFFERED range. The first AAC frame
// played after dropped frames is muted, as shairport-sync mutes the first
// block after a discontinuity: decoded across the gap it would click.
func (p *Player) acceptFrame(value interface{}) bool {
	frame := value.(*PCMFrame)
	seq := frame.SequenceNumber & SeqMask
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	dropped := false
	kept := p.flushes[:0]
	for _, request := range p.flushes {
		if request.drops(seq) {
			dropped = true
		}
		// TCP buffered frames arrive in sequence: once a frame at or after
		// the end arrives, the request is complete.
		if seqDiff(seq, request.until) < 0 {
			kept = append(kept, request)
		}
	}
	clear(p.flushes[len(kept):])
	p.flushes = kept
	if dropped {
		p.flushDropped = true
		return false
	}
	if p.flushDropped {
		p.flushDropped = false
		frame.muteAAC()
	}
	return true
}

func (p *Player) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frame = nil
	p.reportedOutput = false
	p.havePlayout, p.clockOutliers = false, 0
	p.syncError, p.stuffing = 0, 0
	p.resetCatchUp()
	p.flushMu.Lock()
	p.flushes = p.flushes[:0]
	p.flushDropped = false
	p.flushMu.Unlock()
	p.ringBuffer.Reset()
}

// clearAnchor stops timed playback until the next anchor arrives, as
// shairport-sync's reset_ptp_anchor_info. Output keeps running (silent).
func (p *Player) clearAnchor() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clock.anchored = false
	p.havePlayout, p.clockOutliers = false, 0
}
