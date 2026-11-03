package audio

import (
	"fmt"
	"strings"
	"time"
)

// StreamInfo describes the audio stream currently connected, for the
// window's monitor panel.
type StreamInfo struct {
	// Kind is "buffered" (AirPlay 2 AAC over TCP), "realtime" (AirPlay 2
	// ALAC over UDP), "classic" (AirPlay 1) or empty when nothing is connected.
	Kind  string
	Codec string // "aac", "alac" or "l16"
	// Latency is the playback latency the sender asked for (realtime and
	// AirPlay 1), or how far ahead it scheduled the first sample (buffered).
	Latency time.Duration
	// Rate is the sample rate the sender encodes at, which the player and,
	// by default, the output follow. Bits is the sample size of a lossless
	// stream; 0 when unknown or lossy. Both are 0 until the first packet is
	// decoded.
	Rate, Bits int
}

func (p *Player) setStream(kind, codec string) {
	p.mu.Lock()
	p.info = StreamInfo{Kind: kind, Codec: codec}
	p.mu.Unlock()
}

func (p *Player) clearStream() { p.setStream("", "") }

func (p *Player) setLatency(d time.Duration) {
	p.mu.Lock()
	p.info.Latency = d
	p.mu.Unlock()
}

func (p *Player) setFormat(rate, bits int) {
	p.mu.Lock()
	p.info.Rate, p.info.Bits = rate, bits
	p.mu.Unlock()
}

// Monitor returns one "@stats key=value ..." line describing the stream and
// what happened since the previous call. The window reads it; it is not
// logged. Counts in it are also kept for the next Statistics line.
func (p *Player) Monitor() string {
	p.statsMu.Lock()
	fresh := p.take()
	p.unlogged.merge(fresh)
	cur := p.unmonitored
	p.unmonitored = period{}
	p.monitored = true
	cur.merge(fresh)
	now := time.Now()
	received := p.audioBytes.Swap(0)
	var kbps int64
	if !p.bytesAt.IsZero() {
		if elapsed := now.Sub(p.bytesAt); elapsed > 0 {
			kbps = int64(float64(received) * 8 / 1000 / elapsed.Seconds())
		}
	}
	p.bytesAt = now
	p.statsMu.Unlock()
	s, n := cur.statistics, cur.net

	p.mu.Lock()
	info := p.info
	synchronized := p.synchronized
	smoothed := p.syncError
	rate := int(p.rate())
	backlog := p.backlog()
	p.mu.Unlock()
	clockLocked := p.clock.virtualClock != nil && p.clock.virtualClock.Synchronized()
	var delay time.Duration
	if p.clock.virtualClock != nil {
		delay = p.clock.virtualClock.OutputDelay()
	}

	msf := func(samples float64) string { return fmt.Sprintf("%.2f", samples*1000/float64(rate)) }
	kind, codec := info.Kind, info.Codec
	if kind == "" {
		kind, codec = "idle", "-"
	}
	var b strings.Builder
	b.WriteString("@stats")
	fmt.Fprintf(&b, " mode=%s codec=%s sync=%t clock=%t", kind, codec, synchronized, clockLocked)
	fmt.Fprintf(&b, " rate=%d bits=%d kbps=%d", info.Rate, info.Bits, kbps)
	fmt.Fprintf(&b, " latency=%d delay=%d buffer=%s", info.Latency.Milliseconds(), delay.Milliseconds(), msf(float64(backlog)))
	fmt.Fprintf(&b, " output=%d silent=%d", s.frames, s.silent)
	if synchronized && s.errCount > 0 {
		fmt.Fprintf(&b, " err=%s errmin=%s errmax=%s smoothed=%s",
			msf(float64(s.errSum)/float64(s.errCount)), msf(float64(s.errMin)), msf(float64(s.errMax)), msf(smoothed))
	}
	fmt.Fprintf(&b, " added=%d removed=%d resyncs=%d dropped=%d catchup=%d", s.inserted, s.removed, s.resyncs, s.late, s.compressed)
	if s.leadSeen {
		fmt.Fprintf(&b, " leadmin=%d leadmax=%d", s.leadMin.Milliseconds(), s.leadMax.Milliseconds())
	}
	fmt.Fprintf(&b, " rx=%d miss=%d late=%d req=%d rec=%d full=%d", n.received, n.missing, n.late, n.requested, n.recovered, n.full)
	return b.String()
}
