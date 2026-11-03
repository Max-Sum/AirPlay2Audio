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

// recordingSource plays silence and records what the stream asks for.
type recordingSource struct {
	mu                 sync.Mutex
	format             Format
	frames             int
	minAhead, maxAhead time.Duration
}

func (r *recordingSource) Format() Format {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.format
}

func (r *recordingSource) setFormat(f Format) {
	r.mu.Lock()
	r.format = f
	r.mu.Unlock()
}

func (r *recordingSource) Fill(out []int32, _ time.Duration, ahead time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frames == 0 || ahead < r.minAhead {
		r.minAhead = ahead
	}
	if r.frames == 0 || ahead > r.maxAhead {
		r.maxAhead = ahead
	}
	r.frames += len(out) / 2
}

// Plays silence on the default device and checks the player is asked for
// audio in steady blocks, with the reported delay near the queue size.
func TestWASAPIPlaysOnDefaultDevice(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time test")
	}
	saved := Output
	defer func() { Output = saved }()
	Output = OutputConfig{Dither: "auto", API: "wasapi", BufferMs: DefaultBufferMs}

	source := &recordingSource{format: Format{Rate: 44100, Bits: 16}}
	s := newWASAPIStream(newSoftVolume())
	if err := s.Init(source); err != nil {
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

	source.mu.Lock()
	// About 1.2 s played; allow for start-up and the prefilled queue.
	if want := 44100; source.frames < want*8/10 || source.frames > want*2 {
		t.Errorf("player asked for %d frames in about 1.2 s", source.frames)
	}
	if source.maxAhead > 200*time.Millisecond {
		t.Errorf("queued delay up to %s; expected about %d ms plus engine latency", source.maxAhead, DefaultBufferMs)
	}
	t.Logf("frames=%d ahead=%s..%s", source.frames, source.minAhead, source.maxAhead)

	// A new stream format reopens the device in that format, and playback
	// goes on at the new rate.
	source.frames = 0
	source.mu.Unlock()
	source.setFormat(Format{Rate: 48000, Bits: 24})
	time.Sleep(500 * time.Millisecond)
	source.mu.Lock()
	frames := source.frames
	source.mu.Unlock()
	if frames < 48000*3/10 || frames > 48000 {
		t.Errorf("after the format change the player was asked for %d frames in 0.5 s", frames)
	}
	// Once stopped, the device thread waits for a command and leaves the
	// format alone.
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if f := s.format; f != (Format{Rate: 48000, Bits: 24}) {
		t.Errorf("device format %v after the stream changed to 48 kHz 24-bit", f)
	}
}
