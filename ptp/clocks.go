package ptp

import (
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// Several senders may be connected at once, each the master of its own PTP
// clock, and Apple devices send PTP over every address they have (IPv4,
// IPv6 global and link-local). The receiver answers all of them and
// measures each clock separately for each address it arrives from, since
// the paths do not share sequence numbers or delays. Only one clock drives
// the VirtualClock: the clock named by the playing sender's anchors once it
// has been measured, otherwise a measured clock arriving from the playing
// sender's addresses. Switching senders then applies a measurement that is
// already current instead of starting from nothing.

// A measurement older than this is not trusted to drive the clock.
const freshMeasurement = 5 * time.Second

// Paths not heard from for this long are forgotten.
const stalePath = time.Minute

// At most this many clock paths are tracked.
const maxPaths = 64

// How often the per-path counters are logged.
const reportInterval = 10 * time.Second

type pathKey struct {
	id uint64
	ip string
}

type clockState struct {
	id       uint64
	src      net.IP
	measures *measures
	offset   time.Duration // latest local-minus-master measurement
	at       time.Time     // when it was measured
	have     bool
	seen     time.Time
	// Counters since the last report.
	syncs, followUps, delayResps, results int
}

type clockTracker struct {
	mu         sync.Mutex
	clocks     map[pathKey]*clockState
	sources    *[]net.IP // playing sender's addresses; nil follows any clock
	peers      []net.IP  // every connected sender's addresses, for the report
	preferred  uint64      // clock named by the playing sender's anchors
	applied    uint64      // clock currently driving the VirtualClock
	path       *clockState // the path whose measurements drive it
	lastReport time.Time
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, item := range list {
		if item.Equal(ip) {
			return true
		}
	}
	return false
}

func (t *clockTracker) pathLocked(id uint64, src net.IP, now time.Time) *clockState {
	if t.clocks == nil {
		t.clocks = make(map[pathKey]*clockState)
	}
	key := pathKey{id, src.String()}
	c := t.clocks[key]
	if c == nil {
		for other, old := range t.clocks {
			if now.Sub(old.seen) > stalePath {
				delete(t.clocks, other)
			}
		}
		if len(t.clocks) >= maxPaths {
			return nil
		}
		c = &clockState{id: id, src: src, measures: newMeasurements()}
		t.clocks[key] = c
		log.Printf("PTP clock %016x seen from %s", id, src)
	}
	c.seen = now
	return c
}

func fresh(c *clockState, now time.Time) bool {
	return c.have && now.Sub(c.at) <= freshMeasurement
}

// driverLocked returns the measured path that should drive the VirtualClock.
func (t *clockTracker) driverLocked(now time.Time) *clockState {
	if t.preferred != 0 {
		if c := t.bestPathLocked(func(c *clockState) bool { return c.id == t.preferred }, now); c != nil {
			return c
		}
	}
	if t.sources != nil && len(*t.sources) == 0 {
		return nil // AirPlay 1: the clock follows NTP
	}
	// A clock the playing sender sends from any of its addresses.
	ids := map[uint64]bool{}
	for _, c := range t.clocks {
		if t.sources == nil || containsIP(*t.sources, c.src) {
			ids[c.id] = true
		}
	}
	return t.bestPathLocked(func(c *clockState) bool { return ids[c.id] }, now)
}

// bestPathLocked keeps the current path while it is fresh, so measurements
// of one clock over IPv4 and IPv6 do not alternate; otherwise it takes the
// most recently measured matching path.
func (t *clockTracker) bestPathLocked(match func(*clockState) bool, now time.Time) *clockState {
	if t.path != nil && match(t.path) && fresh(t.path, now) && t.clocks[pathKey{t.path.id, t.path.src.String()}] == t.path {
		return t.path
	}
	var best *clockState
	for _, c := range t.clocks {
		if match(c) && fresh(c, now) && (best == nil || c.at.After(best.at)) {
			best = c
		}
	}
	return best
}

// switchLocked moves the VirtualClock to the driving clock at once when it
// changed.
func (t *clockTracker) switchLocked(clock *VirtualClock, now time.Time) {
	d := t.driverLocked(now)
	if d == nil {
		if t.sources != nil && len(*t.sources) == 0 {
			t.applied, t.path = 0, nil
		}
		return
	}
	t.path = d
	if d.id == t.applied {
		return
	}
	clock.Step(d.offset)
	t.applied = d.id
	log.Printf("PTP following clock %016x via %s", d.id, d.src)
}

// measuredLocked records a new measurement and applies it if its clock
// drives the VirtualClock.
func (t *clockTracker) measuredLocked(c *clockState, offset time.Duration, clock *VirtualClock, now time.Time) {
	if !c.have {
		log.Printf("PTP clock %016x via %s measured", c.id, c.src)
	}
	c.offset, c.at, c.have = offset, now, true
	c.results++
	d := t.driverLocked(now)
	if d != c {
		return
	}
	t.path = c
	if t.applied != c.id {
		clock.Step(offset)
		t.applied = c.id
		log.Printf("PTP following clock %016x via %s", c.id, c.src)
		return
	}
	clock.SetLocalOffset(offset)
}

// reportLocked logs what each path did since the last report.
func (t *clockTracker) reportLocked(now time.Time) {
	if now.Sub(t.lastReport) < reportInterval {
		return
	}
	first := t.lastReport.IsZero()
	t.lastReport = now
	if first {
		return
	}
	var lines []string
	for _, c := range t.clocks {
		if c.syncs+c.followUps+c.delayResps == 0 {
			continue
		}
		role := ""
		if c.id == t.applied {
			role = " following"
		}
		if t.sources != nil && containsIP(*t.sources, c.src) {
			role += " playing-sender"
		} else if containsIP(t.peers, c.src) {
			role += " connected-sender"
		}
		lines = append(lines, fmt.Sprintf("PTP clock %016x via %s:%s sync=%d follow-up=%d delay-resp=%d measured=%d",
			c.id, c.src, role, c.syncs, c.followUps, c.delayResps, c.results))
		c.syncs, c.followUps, c.delayResps, c.results = 0, 0, 0, 0
	}
	sort.Strings(lines)
	if len(lines) > 0 {
		log.Print(strings.Join(lines, "\n"))
	}
}

// SetSources selects the playing sender's addresses; its clock drives the
// VirtualClock. An empty list stops following PTP (AirPlay 1 uses NTP).
func (s *Server) SetSources(ips []net.IP) {
	t := &s.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sources != nil && sameSources(*t.sources, ips) {
		return // keep the clock already followed
	}
	copied := append([]net.IP(nil), ips...)
	sameSender := false
	if t.sources != nil {
		for _, ip := range copied {
			sameSender = sameSender || containsIP(*t.sources, ip)
		}
	}
	t.sources = &copied
	if !sameSender {
		// The preference was named by the previous sender's anchors.
		t.preferred = 0
	}
	t.switchLocked(s.clock, time.Now())
}

// SetPeers lists every connected sender's addresses, for the report.
func (s *Server) SetPeers(ips []net.IP) {
	t := &s.tracker
	t.mu.Lock()
	t.peers = append([]net.IP{}, ips...)
	t.mu.Unlock()
}

// PreferClock follows the clock a sender names in its timing anchors, as
// soon as that clock is measured. Zero clears the preference.
func (s *Server) PreferClock(id uint64) {
	t := &s.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.preferred == id {
		return
	}
	t.preferred = id
	known := false
	for _, c := range t.clocks {
		known = known || c.id == id && c.have
	}
	if id != 0 && !known {
		log.Printf("PTP anchor names clock %016x, not measured yet", id)
	}
	t.switchLocked(s.clock, time.Now())
}
