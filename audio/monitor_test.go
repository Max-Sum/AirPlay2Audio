package audio

import (
	"strings"
	"testing"
	"time"

	"github.com/Max-Sum/AirPlay2Audio/ptp"
)

func TestMonitorReportsStreamAndKeepsCountsForStatistics(t *testing.T) {
	p := &Player{clock: &Clock{virtualClock: ptp.NewVirtualClock(120)}, ringBuffer: NewRing(4), synchronized: true, sync: DefaultSyncOptions}
	if line := p.Monitor(); !strings.Contains(line, "mode=idle") || !strings.Contains(line, "delay=120") {
		t.Fatalf("idle monitor line: %q", line)
	}
	p.setStream("realtime", "alac")
	p.setLatency(1750 * time.Millisecond)
	p.setFormat(48000, 24)
	p.audioBytes.Add(32000)
	p.bytesAt = time.Now().Add(-time.Second)
	p.stats.output(1000, 900)
	p.stats.observeError(44)
	p.stats.observeError(-44)
	p.stats.observeLead(300 * time.Millisecond)
	p.AddNetwork(10, 2, 1, 2, 1, 0)
	line := p.Monitor()
	for _, want := range []string{"mode=realtime", "codec=alac", "rate=48000 bits=24 kbps=2", "latency=1750", "latency=1750", "err=0.00", "errmin=-1.00", "errmax=1.00", "leadmin=300", "rx=10", "miss=2", "rec=1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("monitor line lacks %q: %q", want, line)
		}
	}
	// A second read starts from zero.
	if line := p.Monitor(); !strings.Contains(line, "rx=0") || strings.Contains(line, "err=") {
		t.Fatalf("monitor not reset: %q", line)
	}
	// The statistics log still sees everything the monitor read.
	stats, ok := p.Statistics(time.Second)
	if !ok || !strings.Contains(stats, "packets=10 missing=2") || !strings.Contains(stats, "min=-1.00ms max=+1.00ms") {
		t.Fatalf("statistics after monitor: %q", stats)
	}
	p.clearStream()
	if line := p.Monitor(); !strings.Contains(line, "mode=idle") {
		t.Fatalf("stream not cleared: %q", line)
	}
}

// The 10 s statistics line and the 1 s monitor tick can fire together. When
// the log line reads first, the monitor must still see that second's audio
// rather than report playback as paused.
func TestStatisticsDoNotEmptyMonitor(t *testing.T) {
	p := &Player{clock: &Clock{virtualClock: ptp.NewVirtualClock(0)}, ringBuffer: NewRing(4), synchronized: true, sync: DefaultSyncOptions}
	p.setStream("realtime", "alac")
	p.Monitor()
	p.stats.output(44100, 44100)
	p.AddNetwork(125, 0, 0, 0, 0, 0)
	stats, ok := p.Statistics(10 * time.Second)
	if !ok || !strings.Contains(stats, "output=44100") || !strings.Contains(stats, "packets=125") {
		t.Fatalf("statistics: %q", stats)
	}
	line := p.Monitor()
	if !strings.Contains(line, "output=44100") || !strings.Contains(line, "rx=125") {
		t.Fatalf("monitor lost the counts the log took: %q", line)
	}
	// Each count is reported once to each reader.
	if stats, _ := p.Statistics(time.Second); strings.Contains(stats, "packets=125") {
		t.Fatalf("statistics counted twice: %q", stats)
	}
	if line := p.Monitor(); !strings.Contains(line, "output=0") || !strings.Contains(line, "rx=0") {
		t.Fatalf("monitor counted twice: %q", line)
	}
}

func TestStatisticsMerge(t *testing.T) {
	var a, b statistics
	a.observeError(10)
	b.observeError(-5)
	b.observeError(20)
	a.merge(b)
	if a.errCount != 3 || a.errSum != 25 || a.errMin != -5 || a.errMax != 20 {
		t.Fatalf("merged: %+v", a)
	}
	var c statistics
	c.merge(b)
	if c.errCount != 2 || c.errSum != 15 {
		t.Fatalf("merged into empty: %+v", c)
	}
}
