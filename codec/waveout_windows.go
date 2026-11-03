//go:build windows

package codec

import (
	"fmt"
	"log"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	waveSourceFrames = 512 // player block, 44.1 kHz
	waveBuffers      = 5   // ~58 ms, providing room for Windows scheduling jitter
	waveHeaderDone   = 1
	waveMapper       = 0xffffffff
)

var (
	winmm          = syscall.NewLazyDLL("winmm.dll")
	waveOpen       = winmm.NewProc("waveOutOpen")
	wavePrepare    = winmm.NewProc("waveOutPrepareHeader")
	waveUnprepare  = winmm.NewProc("waveOutUnprepareHeader")
	waveWrite      = winmm.NewProc("waveOutWrite")
	waveReset      = winmm.NewProc("waveOutReset")
	waveClose      = winmm.NewProc("waveOutClose")
	wavePosition   = winmm.NewProc("waveOutGetPosition")
	waveNumDevs    = winmm.NewProc("waveOutGetNumDevs")
	waveDevCaps    = winmm.NewProc("waveOutGetDevCapsW")
	kernel         = syscall.NewLazyDLL("kernel32.dll")
	createEvent    = kernel.NewProc("CreateEventW")
	waitEvent      = kernel.NewProc("WaitForSingleObject")
	closeHandle    = kernel.NewProc("CloseHandle")
	getProcessHeap = kernel.NewProc("GetProcessHeap")
	heapAlloc      = kernel.NewProc("HeapAlloc")
	heapFree       = kernel.NewProc("HeapFree")
)

// waveFormatExtensible starts with WAVEFORMATEX. The plain 16-bit format
// uses only the first 18 bytes.
type waveFormatExtensible struct {
	Tag, Channels                        uint16
	SamplesPerSec, AvgBytesPerSec        uint32
	BlockAlign, BitsPerSample, ExtraSize uint16
	ValidBits                            uint16
	ChannelMask                          uint32
	SubFormat                            [16]byte
}

// KSDATAFORMAT_SUBTYPE_PCM
var subtypePCM = [16]byte{0x01, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}

// pcmFormat describes interleaved stereo integer PCM; sizes above 16 bits
// need WAVE_FORMAT_EXTENSIBLE.
func pcmFormat(rate, bits int) waveFormatExtensible {
	frameBytes := 2 * bits / 8
	format := waveFormatExtensible{
		Tag: 1, Channels: 2, SamplesPerSec: uint32(rate),
		AvgBytesPerSec: uint32(rate * frameBytes), BlockAlign: uint16(frameBytes), BitsPerSample: uint16(bits),
	}
	if bits > 16 {
		format.Tag, format.ExtraSize = 0xfffe, 22
		format.ValidBits, format.ChannelMask, format.SubFormat = uint16(bits), 3, subtypePCM
	}
	return format
}

type waveHeader struct {
	Data             uintptr
	Length, Recorded uint32
	User             uintptr
	Flags, Loops     uint32
	Next, Reserved   uintptr
}
type multimediaTime struct{ Kind, Value, Reserved uint32 }
type waveBuffer struct {
	header *waveHeader
	memory uintptr
	queued bool
}

// waveOutCaps is WAVEOUTCAPSW.
type waveOutCaps struct {
	Mid, Pid      uint16
	DriverVersion uint32
	Name          [32]uint16
	Formats       uint32
	Channels      uint16
	Reserved      uint16
	Support       uint32
}

// Devices lists waveOut devices. Windows truncates their names to 31
// characters.
func waveOutDevices() ([]Device, error) {
	if err := waveNumDevs.Find(); err != nil {
		return nil, err
	}
	count, _, _ := waveNumDevs.Call()
	devices := make([]Device, 0, count)
	for i := uintptr(0); i < count; i++ {
		var caps waveOutCaps
		if code, _, _ := waveDevCaps.Call(i, uintptr(unsafe.Pointer(&caps)), unsafe.Sizeof(caps)); code != 0 {
			continue
		}
		devices = append(devices, Device{Index: int(i), Name: syscall.UTF16ToString(caps.Name[:])})
	}
	return devices, nil
}

type WaveOutStream struct {
	device, event, heap uintptr
	config              OutputConfig
	frames              int // output frames per buffer
	frameBytes          int
	buffers             []waveBuffer
	converter           *converter
	callback            StreamCallback
	*softVolume
	mu         sync.Mutex
	stop, done chan struct{}
}

func newWaveOutStream(volume *softVolume) *WaveOutStream {
	return &WaveOutStream{softVolume: volume}
}

func waveError(operation string, code uintptr) error {
	if code == 0 {
		return nil
	}
	return fmt.Errorf("%s: Windows audio error %d", operation, code)
}

func (s *WaveOutStream) Init(callback StreamCallback) error {
	s.callback = callback
	s.config = Output
	if err := s.config.Validate(); err != nil {
		return err
	}
	s.heap, _, _ = getProcessHeap.Call()
	s.event, _, _ = createEvent.Call(0, 0, 0, 0)
	if s.heap == 0 || s.event == 0 {
		return fmt.Errorf("cannot allocate Windows audio event")
	}
	if err := s.open(); err != nil {
		s.Close()
		return err
	}
	return nil
}

// open opens the configured device. It is called at Init and again by Start
// after Release, so a session that starts later follows the current
// Windows default device.
func (s *WaveOutStream) open() error {
	id, name := uintptr(waveMapper), "system default"
	if s.config.Device != "" {
		devices, err := waveOutDevices()
		if err != nil {
			return err
		}
		device, err := matchDevice(s.config.Device, devices)
		if err != nil {
			return err
		}
		id, name = uintptr(device.Index), device.Name
	}
	s.frameBytes = 2 * s.config.Bits / 8
	format := pcmFormat(s.config.Rate, s.config.Bits)
	code, _, _ := waveOpen.Call(uintptr(unsafe.Pointer(&s.device)), id, uintptr(unsafe.Pointer(&format)), s.event, 0, 0x50000) // CALLBACK_EVENT
	if err := waveError(fmt.Sprintf("open output %q at %d Hz %d-bit", name, s.config.Rate, s.config.Bits), code); err != nil {
		s.device = 0
		return err
	}
	// Keep the buffer duration (and so the queue depth) independent of rate.
	s.frames = waveSourceFrames * s.config.Rate / SourceRate
	s.converter = newConverter(s.config, waveSourceFrames)
	for i := 0; i < waveBuffers; i++ {
		size := uintptr(s.frames * s.frameBytes)
		h, _, _ := heapAlloc.Call(s.heap, 8, unsafe.Sizeof(waveHeader{}))
		mem, _, _ := heapAlloc.Call(s.heap, 8, size)
		if h == 0 || mem == 0 {
			if h != 0 {
				heapFree.Call(s.heap, 0, h)
			}
			if mem != 0 {
				heapFree.Call(s.heap, 0, mem)
			}
			s.closeDevice()
			return fmt.Errorf("cannot allocate Windows audio buffers")
		}
		header := (*waveHeader)(unsafe.Pointer(h))
		header.Data, header.Length = mem, uint32(size)
		code, _, _ := wavePrepare.Call(s.device, h, unsafe.Sizeof(waveHeader{}))
		if err := waveError("prepare output buffer", code); err != nil {
			heapFree.Call(s.heap, 0, h)
			heapFree.Call(s.heap, 0, mem)
			s.closeDevice()
			return err
		}
		s.buffers = append(s.buffers, waveBuffer{header: header, memory: mem})
	}
	log.Printf("AUDIO output device=%q format=%d Hz %d-bit dither=%s", name, s.config.Rate, s.config.Bits, s.config.Dither)
	return nil
}

func (s *WaveOutStream) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.device == 0 {
		if s.event == 0 {
			return fmt.Errorf("Windows audio output is not initialized")
		}
		if err := s.open(); err != nil {
			return err
		}
	}
	if s.stop != nil {
		return nil
	}
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	go s.run(s.stop, s.done)
	return nil
}

func (s *WaveOutStream) run(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	defer waveReset.Call(s.device)
	rate := uint64(s.config.Rate)
	var written uint64
	for {
		select {
		case <-stop:
			return
		default:
		}
		for i := range s.buffers {
			b := &s.buffers[i]
			if b.queued && b.header.Flags&waveHeaderDone == 0 {
				continue
			}
			b.queued = false
			pos := multimediaTime{Kind: 2} // TIME_SAMPLES, per channel
			code, _, _ := wavePosition.Call(s.device, uintptr(unsafe.Pointer(&pos)), unsafe.Sizeof(pos))
			played := uint64(0)
			if code == 0 {
				switch pos.Kind {
				case 2:
					played = uint64(pos.Value)
				case 4:
					played = uint64(pos.Value) / uint64(s.frameBytes)
				case 1:
					played = uint64(pos.Value) * rate / 1000
				}
			}
			queued := uint64(0)
			if written > played {
				queued = written - played
			}
			if limit := uint64(waveBuffers * s.frames); queued > limit {
				queued = limit
			}
			out := unsafe.Slice((*byte)(unsafe.Pointer(b.memory)), s.frames*s.frameBytes)
			ahead := time.Duration(queued) * time.Second / time.Duration(rate)
			s.converter.fill(out, s.frames, s.gain(), func(src []int16) {
				// Audio taken now plays after what is queued at the device and
				// what the resampler still holds.
				pending := time.Duration(s.converter.pendingSourceFrames() * float64(time.Second) / SourceRate)
				s.callback(src, 0, ahead+pending)
			})
			code, _, _ = waveWrite.Call(s.device, uintptr(unsafe.Pointer(b.header)), unsafe.Sizeof(waveHeader{}))
			if err := waveError("write audio", code); err != nil {
				log.Print(err)
				return
			}
			b.queued = true
			written += uint64(s.frames)
		}
		waitEvent.Call(s.event, 10)
	}
}

func (s *WaveOutStream) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	return nil
}

func (s *WaveOutStream) stopLocked() {
	if s.stop != nil {
		close(s.stop)
		<-s.done
		s.stop, s.done = nil, nil
	}
	for i := range s.buffers {
		s.buffers[i].queued = false
	}
}

// Release stops playback and closes the device until the next Start.
func (s *WaveOutStream) Release() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	s.closeDevice()
	return nil
}

func (s *WaveOutStream) closeDevice() {
	for _, b := range s.buffers {
		if s.device != 0 {
			waveUnprepare.Call(s.device, uintptr(unsafe.Pointer(b.header)), unsafe.Sizeof(waveHeader{}))
		}
		heapFree.Call(s.heap, 0, b.memory)
		heapFree.Call(s.heap, 0, uintptr(unsafe.Pointer(b.header)))
	}
	s.buffers = nil
	if s.device != 0 {
		waveClose.Call(s.device)
		s.device = 0
	}
}

func (s *WaveOutStream) Close() error {
	s.Stop()
	s.mu.Lock()
	s.closeDevice()
	s.mu.Unlock()
	if s.event != 0 {
		closeHandle.Call(s.event)
		s.event = 0
	}
	return nil
}
