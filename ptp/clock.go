package ptp

import (
	"sync"
	"time"
)

// VirtualClock maps the local monotonic clock onto the sender's PTP clock.
type VirtualClock struct {
	mu           sync.RWMutex
	start        time.Time
	offset       time.Duration
	delay        time.Duration
	synchronized bool
	pending      time.Duration
	pendingCount int
}

func NewVirtualClock(delayMS int64) *VirtualClock {
	return &VirtualClock{start: time.Now(), delay: time.Duration(delayMS) * time.Millisecond}
}

func (v *VirtualClock) Offset(diff time.Duration) {
	v.mu.Lock()
	// Smooth normal packet-arrival jitter. Large changes indicate a new clock
	// or initial lock and must converge immediately.
	if v.synchronized && diff > -250*time.Millisecond && diff < 250*time.Millisecond {
		diff /= 8
	}
	v.offset -= diff
	v.synchronized = true
	v.mu.Unlock()
}

// LocalNow shares Now's monotonic origin but is unaffected by PTP corrections.
// Measurements must use a stable timebase throughout an exchange.
func (v *VirtualClock) LocalNow() time.Time {
	return time.Unix(0, int64(time.Since(v.start)))
}

// SetLocalOffset applies an absolute local-minus-master measurement.
func (v *VirtualClock) SetLocalOffset(localMinusMaster time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	diff := localMinusMaster + v.offset
	if v.synchronized {
		if diff <= -250*time.Millisecond || diff >= 250*time.Millisecond {
			// A single delayed packet is not evidence that the master clock
			// stepped. Require three consistent large observations first.
			change := localMinusMaster - v.pending
			if v.pendingCount == 0 || change < -50*time.Millisecond || change > 50*time.Millisecond {
				v.pending = localMinusMaster
				v.pendingCount = 1
			} else {
				v.pendingCount++
			}
			if v.pendingCount < 3 {
				return
			}
		} else {
			diff /= 8
		}
	}
	v.offset -= diff
	v.synchronized = true
	v.pendingCount = 0
}

// Step applies a local-minus-master measurement at once, without the
// smoothing and confirmation SetLocalOffset uses, for a switch to a clock
// that has already been measured.
func (v *VirtualClock) Step(localMinusMaster time.Duration) {
	v.mu.Lock()
	v.offset, v.synchronized, v.pendingCount = -localMinusMaster, true, 0
	v.mu.Unlock()
}

func (v *VirtualClock) Now() time.Time {
	v.mu.RLock()
	elapsed := time.Since(v.start) + v.offset
	v.mu.RUnlock()
	return time.Unix(0, int64(elapsed))
}

// CurrentOffset is master time minus local time.
func (v *VirtualClock) CurrentOffset() time.Duration {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.offset
}

// Reset forgets the synchronization, for a sender using another timing
// protocol (NTP instead of PTP). Until it is measured again the clock reports
// itself unsynchronized and timed playback waits.
func (v *VirtualClock) Reset() {
	v.mu.Lock()
	v.offset, v.synchronized, v.pendingCount = 0, false, 0
	v.mu.Unlock()
}

// OutputDelay is a physical playback-path correction; it must not be included
// in the PTP measurements or clock synchronization would cancel it out.
func (v *VirtualClock) OutputDelay() time.Duration { return v.delay }

func (v *VirtualClock) Synchronized() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.synchronized
}
