//go:build windows && (amd64 || arm64)

package codec

import (
	"sync"
	"testing"
	"time"
)

func TestWASAPIListsDevices(t *testing.T) {
	devices, err := wasapiDevices()
	if err != nil {
		t.Skipf("no WASAPI on this machine: %v", err)
	}
	for _, d := range devices {
		if d.Name == "" {
			t.Errorf("device %d has no name", d.Index)
		}
	}
	t.Logf("%d output devices: %v", len(devices), devices)
}

// Plays silence on the default device and checks the player is asked for
// audio in steady blocks, with the reported delay near the queue size.
func TestWASAPIPlaysOnDefaultDevice(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time test")
	}
	saved := Output
	defer func() { Output = saved }()
	Output = OutputConfig{Bits: 16, Rate: SourceRate, Dither: "auto", API: "wasapi", BufferMs: DefaultBufferMs}

	var mu sync.Mutex
	var frames int
	var minAhead, maxAhead time.Duration
	s := newWASAPIStream(newSoftVolume())
	err := s.Init(func(out []int16, _ time.Duration, ahead time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		if frames == 0 || ahead < minAhead {
			minAhead = ahead
		}
		if frames == 0 || ahead > maxAhead {
			maxAhead = ahead
		}
		frames += len(out) / 2
	})
	if err != nil {
		t.Skipf("no default output device: %v", err)
	}
	defer s.Close()
	for round := 0; round < 2; round++ { // a second Start after Stop must work
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Release(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil { // reopens after Release
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	// About 1.2 s played; allow for start-up and the prefilled queue.
	if want := int(SourceRate) * 1; frames < want*8/10 || frames > want*2 {
		t.Errorf("player asked for %d frames in about 1.2 s", frames)
	}
	if maxAhead > 200*time.Millisecond {
		t.Errorf("queued delay up to %s; expected about %d ms plus engine latency", maxAhead, DefaultBufferMs)
	}
	t.Logf("frames=%d ahead=%s..%s", frames, minAhead, maxAhead)
}
