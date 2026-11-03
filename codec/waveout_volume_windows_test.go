package codec

import (
	"math"
	"testing"
)

func TestLocalAndAirPlayVolumeCompose(t *testing.T) {
	s := newSoftVolume()
	if err := s.SetVolume(-20); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLocalVolume(0.5); err != nil {
		t.Fatal(err)
	}
	if g := s.gain(); math.Abs(g-0.05) > 1e-9 {
		t.Fatalf("remote and local gain should multiply: %v", g)
	}
	if err := s.SetLocalVolume(0); err != nil {
		t.Fatal(err)
	}
	if s.gain() != 0 {
		t.Fatal("local mute should silence samples")
	}
	if err := s.SetLocalVolume(1.1); err == nil {
		t.Fatal("out-of-range gain accepted")
	}
}

// Both Windows outputs share the stream's volume, so a fallback keeps it.
func TestStreamForwardsVolume(t *testing.T) {
	s := NewStream().(*windowsStream)
	if err := s.SetVolume(-20); err != nil {
		t.Fatal(err)
	}
	local, ok := Stream(s).(interface{ SetLocalVolume(float64) error })
	if !ok {
		t.Fatal("stream does not offer local volume")
	}
	if err := local.SetLocalVolume(0.5); err != nil {
		t.Fatal(err)
	}
	if g := newWaveOutStream(s.softVolume).gain(); math.Abs(g-0.05) > 1e-9 {
		t.Fatalf("waveOut does not see the stream's volume: %v", g)
	}
}
