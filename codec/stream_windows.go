//go:build windows

package codec

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync/atomic"
	"time"
)

// softVolume is the gain applied to samples before they reach the device:
// the AirPlay sender's volume times this receiver's local volume. It is
// shared by both Windows outputs so a fallback keeps the volume.
type softVolume struct{ volume, localVolume atomic.Uint64 }

func newSoftVolume() *softVolume {
	v := &softVolume{}
	v.volume.Store(math.Float64bits(1))
	v.localVolume.Store(math.Float64bits(1))
	return v
}

func (v *softVolume) SetVolume(db float64) error {
	if math.IsNaN(db) {
		return fmt.Errorf("invalid volume")
	}
	gain := 0.0
	if db > -144 {
		gain = math.Pow(10, math.Min(db, 0)/20)
	}
	v.volume.Store(math.Float64bits(gain))
	return nil
}

// SetLocalVolume controls this receiver only and composes with the AirPlay
// sender's volume. Value 1 is unity gain; value 0 mutes this receiver.
func (v *softVolume) SetLocalVolume(gain float64) error {
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain < 0 || gain > 1 {
		return fmt.Errorf("local volume must be between 0 and 1")
	}
	v.localVolume.Store(math.Float64bits(gain))
	return nil
}

func (v *softVolume) gain() float64 {
	return math.Float64frombits(v.volume.Load()) * math.Float64frombits(v.localVolume.Load())
}

// releaser is implemented by outputs that can close their device while idle.
type releaser interface{ Release() error }

// windowsStream picks the output API when it is initialized: WASAPI unless
// waveOut was asked for, and waveOut when WASAPI cannot be opened.
type windowsStream struct {
	*softVolume
	impl Stream
}

func NewStream() Stream { return &windowsStream{softVolume: newSoftVolume()} }

func useWaveOut() bool { return strings.EqualFold(Output.API, "waveout") }

func (s *windowsStream) Init(callback StreamCallback) error {
	if !useWaveOut() {
		w := newWASAPIStream(s.softVolume)
		err := w.Init(callback)
		if err == nil {
			s.impl = w
			return nil
		}
		log.Printf("AUDIO WASAPI output unavailable (%v); using waveOut", err)
	}
	w := newWaveOutStream(s.softVolume)
	if err := w.Init(callback); err != nil {
		return err
	}
	s.impl = w
	return nil
}

func (s *windowsStream) Start() error {
	if s.impl == nil {
		return fmt.Errorf("Windows audio output is not initialized")
	}
	return s.impl.Start()
}

func (s *windowsStream) Stop() error {
	if s.impl == nil {
		return nil
	}
	return s.impl.Stop()
}

func (s *windowsStream) Release() error {
	if r, ok := s.impl.(releaser); ok {
		return r.Release()
	}
	return nil
}

func (s *windowsStream) Close() error {
	if s.impl == nil {
		return nil
	}
	return s.impl.Close()
}

// Devices lists the output devices of the selected API. WASAPI gives full
// names; waveOut truncates them to 31 characters.
func Devices() ([]Device, error) {
	if !useWaveOut() {
		devices, err := wasapiDevices()
		if err == nil {
			return devices, nil
		}
		log.Printf("AUDIO cannot list WASAPI devices (%v); listing waveOut devices", err)
	}
	return waveOutDevices()
}

// frameTime converts a frame count at rate to a duration.
func frameTime(frames uint64, rate int) time.Duration {
	return time.Duration(frames) * time.Second / time.Duration(rate)
}
