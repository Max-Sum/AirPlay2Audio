package audio

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// statistics are collected by the output callback with the player lock held
// and printed by Statistics, like shairport-sync's statistics option.
type statistics struct {
	frames, silent   uint64 // output samples, and those with no audio
	late             uint64 // source samples dropped because their time had passed
	compressed       uint64 // samples removed by low-latency catch-up
	inserted         uint64 // drift correction blocks played one sample slower
	removed          uint64 // drift correction blocks played one sample faster
	resyncs          uint64
	errSum, errCount int64
	errMin, errMax   int64
	queueMin         int
	queueMax         int
	queueSeen        bool
	// How long before its playback time each realtime packet arrived.
	leadMin, leadMax time.Duration
	leadSeen         bool
}

func (s *statistics) output(frames, written int) {
	s.frames += uint64(frames)
	if written < frames {
		s.silent += uint64(frames - written)
	}
}

func (s *statistics) observeError(samples int64) {
	if s.errCount == 0 || samples < s.errMin {
		s.errMin = samples
	}
	if s.errCount == 0 || samples > s.errMax {
		s.errMax = samples
	}
	s.errSum += samples
	s.errCount++
}

func (s *statistics) observeQueue(n int) {
	if !s.queueSeen || n < s.queueMin {
		s.queueMin = n
	}
	if !s.queueSeen || n > s.queueMax {
		s.queueMax = n
	}
	s.queueSeen = true
}

func (s *statistics) observeLead(lead time.Duration) {
	if !s.leadSeen || lead < s.leadMin {
		s.leadMin = lead
	}
	if !s.leadSeen || lead > s.leadMax {
		s.leadMax = lead
	}
	s.leadSeen = true
}

// merge adds the counts of o, a later period, to s.
func (s *statistics) merge(o statistics) {
	s.frames += o.frames
	s.silent += o.silent
	s.late += o.late
	s.compressed += o.compressed
	s.inserted += o.inserted
	s.removed += o.removed
	s.resyncs += o.resyncs
	if o.errCount > 0 {
		s.observeError(o.errMin)
		s.observeError(o.errMax)
		// observeError counted the two bounds; replace them with o's totals.
		s.errSum += o.errSum - o.errMin - o.errMax
		s.errCount += o.errCount - 2
	}
	if o.queueSeen {
		s.observeQueue(o.queueMin)
		s.observeQueue(o.queueMax)
	}
	if o.leadSeen {
		s.observeLead(o.leadMin)
		s.observeLead(o.leadMax)
	}
}

// netCounters are added to by the network receivers.
type netCounters struct {
	received, missing, late, requested, recovered, full atomic.Uint64
}

type netCounts struct {
	received, missing, late, requested, recovered, full uint64
}

func (n *netCounts) merge(o netCounts) {
	n.received += o.received
	n.missing += o.missing
	n.late += o.late
	n.requested += o.requested
	n.recovered += o.recovered
	n.full += o.full
}

// period holds everything counted between two reads of the statistics.
type period struct {
	statistics
	net netCounts
}

func (p *period) merge(o period) {
	p.statistics.merge(o.statistics)
	p.net.merge(o.net)
}

// take returns and resets the counters. Call with statsMu held.
func (p *Player) take() period {
	p.mu.Lock()
	s := p.stats
	p.stats = statistics{}
	p.mu.Unlock()
	return period{statistics: s, net: netCounts{
		received:  p.net.received.Swap(0),
		missing:   p.net.missing.Swap(0),
		late:      p.net.late.Swap(0),
		requested: p.net.requested.Swap(0),
		recovered: p.net.recovered.Swap(0),
		full:      p.net.full.Swap(0),
	}}
}

// AddNetwork records packet counts from a realtime receiver.
func (p *Player) AddNetwork(received, missing, late, requested, recovered, full uint64) {
	p.net.received.Add(received)
	p.net.missing.Add(missing)
	p.net.late.Add(late)
	p.net.requested.Add(requested)
	p.net.recovered.Add(recovered)
	p.net.full.Add(full)
}

func ms(samples float64) string { return fmt.Sprintf("%+.2fms", samples*1000/float64(sampleRate)) }

// Statistics returns one line describing playback since the previous call,
// or false when nothing was played. elapsed is the time since that call.
func (p *Player) Statistics(elapsed time.Duration) (string, bool) {
	p.statsMu.Lock()
	cur := p.unlogged
	p.unlogged = period{}
	cur.merge(p.take())
	p.statsMu.Unlock()
	s, n := cur.statistics, cur.net
	p.mu.Lock()
	synchronized := p.synchronized
	smoothed := p.syncError
	p.mu.Unlock()
	if s.frames == 0 || s.silent == s.frames && n.received == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString("STATS")
	if synchronized {
		if s.errCount > 0 {
			fmt.Fprintf(&b, " sync-error=%s smoothed=%s min=%s max=%s",
				ms(float64(s.errSum)/float64(s.errCount)), ms(smoothed), ms(float64(s.errMin)), ms(float64(s.errMax)))
		}
		// Net correction rate: what the drift correction did, which once
		// settled is the drift between sender and sound card.
		ppm := (float64(s.removed) - float64(s.inserted)) / float64(s.frames) * 1e6
		fmt.Fprintf(&b, " correction=%+.0fppm (+%d/-%d samples) resyncs=%d late-dropped=%d", ppm, s.removed, s.inserted, s.resyncs, s.late)
	} else {
		fmt.Fprintf(&b, " mode=unsynchronized catch-up-removed=%d", s.compressed)
	}
	if rate, ok := p.clock.drift(elapsed); ok {
		// How fast the sender's clock runs against this computer's clock.
		fmt.Fprintf(&b, " sender-clock=%+.0fppm", rate)
	}
	fmt.Fprintf(&b, " output=%d silent=%d queue=%d..%d", s.frames, s.silent, s.queueMin, s.queueMax)
	if n.received > 0 || n.missing > 0 {
		fmt.Fprintf(&b, " packets=%d missing=%d late=%d resend-requested=%d resend-recovered=%d output-queue-full=%d",
			n.received, n.missing, n.late, n.requested, n.recovered, n.full)
	}
	return b.String(), true
}
