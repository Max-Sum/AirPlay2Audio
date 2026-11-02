package audio

import (
	"goplay2/ptp"
	"time"
)

const sampleRate int64 = 44100

// Clock maps the sender's anchor pair to RTP sample timestamps.
type Clock struct {
	virtualClock  *ptp.VirtualClock
	anchorNetwork int64
	anchorRTP     uint32
	anchored      bool

	// Used only by statistics.
	lastOffset     time.Duration
	haveLastOffset bool
}

// drift reports the change of the sender clock offset since the previous
// call, in parts per million of elapsed.
func (c *Clock) drift(elapsed time.Duration) (float64, bool) {
	if c.virtualClock == nil || !c.virtualClock.Synchronized() || elapsed <= 0 {
		c.haveLastOffset = false
		return 0, false
	}
	offset := c.virtualClock.CurrentOffset()
	previous, had := c.lastOffset, c.haveLastOffset
	c.lastOffset, c.haveLastOffset = offset, true
	change := offset - previous
	// A step (new sender or clock source) is not drift.
	if !had || change > 50*time.Millisecond || change < -50*time.Millisecond {
		return 0, false
	}
	return float64(change) / float64(elapsed) * 1e6, true
}

func (c *Clock) AnchorTime(networkTime, rtpTime int64) {
	c.anchorNetwork = networkTime
	c.anchorRTP = uint32(rtpTime)
	c.anchored = true
}

// AnchorLead reports how far in the future (positive) the sender scheduled the
// anchor sample, measured on the synchronized PTP clock.
func (c *Clock) AnchorLead() (time.Duration, bool) {
	if !c.anchored || c.virtualClock == nil || !c.virtualClock.Synchronized() {
		return 0, false
	}
	return time.Duration(c.anchorNetwork - c.virtualClock.Now().UnixNano()), true
}

func (c *Clock) TargetRTP(outputAhead time.Duration) (uint32, bool) {
	if !c.anchored || !c.virtualClock.Synchronized() {
		return 0, false
	}
	delta := c.virtualClock.Now().Add(outputAhead+c.virtualClock.OutputDelay()).UnixNano() - c.anchorNetwork
	// Ignore invalid anchors instead of overflowing sample arithmetic.
	if delta > int64(24*time.Hour) || delta < -int64(24*time.Hour) {
		return 0, false
	}
	samples := (delta/int64(time.Second))*sampleRate + (delta%int64(time.Second))*sampleRate/int64(time.Second)
	return c.anchorRTP + uint32(samples), true
}
