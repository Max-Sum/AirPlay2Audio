package audio

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Max-Sum/AirPlay2Audio/ptp"
)

// The sender clock runs ppm faster than the sound card. Without correction
// the playout falls behind until it jumps; with it, the error stays within
// the tolerance and the net correction rate matches the drift.
func simulateDrift(t *testing.T, ppm float64, correct bool) (maxErr float64, resyncs uint64, net int64) {
	t.Helper()
	p := &Player{ringBuffer: NewRing(1), sync: DefaultSyncOptions}
	p.sync.DriftCorrection = correct
	const frames = 512
	sender := 1000.0
	for block := 0; block < 44100*120/frames; block++ { // two minutes
		// Noisy reading of the target, as from a quantized device position.
		noise := 40 * math.Sin(float64(block)*1.7)
		target := uint32(int64(sender + noise))
		start, consume := p.planPlayout(target, frames)
		if block > 2000 {
			if e := math.Abs(sender - float64(start)); e > maxErr {
				maxErr = e
			}
		}
		net += int64(consume - frames)
		sender += frames * (1 + ppm/1e6)
	}
	return maxErr, p.stats.resyncs, net
}

func TestDriftCorrectionTracksSenderClock(t *testing.T) {
	tolerance := float64(DefaultSyncOptions.Tolerance) * float64(cdRate) / float64(time.Second)
	for _, ppm := range []float64{300, -300, 50} {
		maxErr, resyncs, net := simulateDrift(t, ppm, true)
		if resyncs != 0 || maxErr > tolerance+50 {
			t.Fatalf("%v ppm: max error %.0f samples, resyncs %d", ppm, maxErr, resyncs)
		}
		expected := ppm / 1e6 * 44100 * 120
		if math.Abs(float64(net)-expected) > tolerance*1.5 {
			t.Fatalf("%v ppm: corrected %d samples, drift was %.0f", ppm, net, expected)
		}
	}
	// Without correction 300 ppm exceeds the 50 ms resync limit in about
	// three minutes; in two minutes the error reaches about 36 ms.
	if maxErr, _, _ := simulateDrift(t, 300, false); maxErr < 1000 {
		t.Fatalf("uncorrected drift unexpectedly small: %.0f", maxErr)
	}
}

func TestStretchedBlockJoinsNeighbours(t *testing.T) {
	p := &Player{ringBuffer: NewRing(2)}
	ramp := make([]int32, 20*2)
	for i := 0; i < 20; i++ {
		ramp[2*i], ramp[2*i+1] = int32(i*100), int32(-i*100)
	}
	p.Push(testPCM(0, 1, ramp...))
	out := make([]int32, 8*2)
	// Consume 9 samples into 8: first and last are kept exactly.
	p.renderStretched(out, 0, 9)
	if out[0] != 0 || out[14] != 800 || out[15] != -800 {
		t.Fatalf("endpoints: %v", out)
	}
	for i := 1; i < 8; i++ {
		if out[2*i] <= out[2*i-2] {
			t.Fatalf("not monotonic: %v", out)
		}
	}
	// The next block continues at sample 9.
	p.renderStretched(out, 9, 8)
	if out[0] != 900 {
		t.Fatalf("next block starts at %d", out[0])
	}
}

func TestStatisticsReportDriftAndNetwork(t *testing.T) {
	p := &Player{clock: &Clock{virtualClock: ptp.NewVirtualClock(0)}, ringBuffer: NewRing(4), synchronized: true, sync: DefaultSyncOptions}
	p.stats.output(1000, 900)
	p.stats.observeError(44)
	p.stats.removed = 3
	p.AddNetwork(10, 2, 1, 2, 1, 0)
	line, ok := p.Statistics(time.Second)
	if !ok || !strings.Contains(line, "sync-error=+1.00ms") || !strings.Contains(line, "missing=2") || !strings.Contains(line, "correction=+3000ppm") {
		t.Fatalf("unexpected statistics: %q", line)
	}
	if _, ok := p.Statistics(time.Second); ok {
		t.Fatal("statistics not reset")
	}
}
