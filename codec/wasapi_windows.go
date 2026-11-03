//go:build windows && (amd64 || arm64)

package codec

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// WASAPI shared-mode output, event driven. Compared with waveOut it keeps
// only about two engine periods queued (20 ms instead of about 58 ms) and
// reports exactly how much is queued, so synchronized playback has more
// room for network jitter and a steadier timing measurement.
//
// COM objects are created and used only on one locked OS thread per stream;
// the public methods hand commands to that thread.

type propertyKey struct {
	fmtid guid
	pid   uint32
}

// propVariant is PROPVARIANT; only the string case is read.
type propVariant struct {
	vt  uint16
	_   [3]uint16
	val uintptr
	_   uintptr
}

var (
	clsidMMDeviceEnumerator = guid{0xBCDE0395, 0xE52F, 0x467C, [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = guid{0xA95664D2, 0x9614, 0x4F35, [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient         = guid{0x1CB9AD4C, 0xDBFA, 0x4C32, [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioRenderClient   = guid{0xF294ACFC, 0x3146, 0x4483, [8]byte{0xA7, 0xBF, 0xAD, 0xDC, 0xA7, 0xC2, 0x60, 0xE2}}
	// PKEY_Device_FriendlyName
	keyFriendlyName = propertyKey{guid{0xA45C254E, 0xDF1C, 0x4EFD, [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}}, 14}

	coInitializeEx   = ole32.NewProc("CoInitializeEx")
	coUninitialize   = ole32.NewProc("CoUninitialize")
	coTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	propVariantClear = ole32.NewProc("PropVariantClear")
	avrt             = syscall.NewLazyDLL("avrt.dll")
	avSetMmThread    = avrt.NewProc("AvSetMmThreadCharacteristicsW")
	avRevertMmThread = avrt.NewProc("AvRevertMmThreadCharacteristics")
)

const (
	clsctxAll               = 0x17
	coinitMultithreaded     = 0
	eRender                 = 0
	eConsole                = 0
	deviceStateActive       = 1
	stgmRead                = 0
	vtLPWSTR                = 31
	sharedMode              = 0
	streamEventCallback     = 0x00040000
	streamAutoConvertPCM    = 0x80000000
	streamSRCDefaultQuality = 0x08000000

	// Method slots, counted from the start of each vtable (IUnknown is 0-2).
	enumEndpoints     = 3 // IMMDeviceEnumerator
	enumDefault       = 4
	collectionCount   = 3 // IMMDeviceCollection
	collectionItem    = 4
	deviceActivate    = 3 // IMMDevice
	deviceProperties  = 4
	deviceGetID       = 5
	propertyGetValue  = 5 // IPropertyStore
	clientInitialize  = 3 // IAudioClient
	clientBufferSize  = 4
	clientLatency     = 5
	clientPadding     = 6
	clientPeriod      = 9
	clientStart       = 10
	clientStop        = 11
	clientReset       = 12
	clientSetEvent    = 13
	clientService     = 14
	renderGetBuffer   = 3 // IAudioRenderClient
	renderFreeBuffer  = 4
	referenceTimeUnit = 100 * time.Nanosecond
)

type comObject uintptr

// call invokes a method of a COM interface.
//
//go:uintptrescapes
func (o comObject) call(slot int, args ...uintptr) uintptr {
	vtable := *(*uintptr)(unsafe.Pointer(o))
	method := *(*uintptr)(unsafe.Pointer(vtable + uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(method, append([]uintptr{uintptr(o)}, args...)...)
	return r
}

func (o *comObject) release() {
	if *o != 0 {
		o.call(2)
		*o = 0
	}
}

// utf16At reads a NUL-terminated UTF-16 string that Windows allocated.
func utf16At(p uintptr) string {
	if p == 0 {
		return ""
	}
	var chars []uint16
	for i := uintptr(0); ; i += 2 {
		c := *(*uint16)(unsafe.Pointer(p + i))
		if c == 0 {
			break
		}
		chars = append(chars, c)
	}
	return syscall.UTF16ToString(chars)
}

func comInit() error {
	r, _, _ := coInitializeEx.Call(0, coinitMultithreaded)
	return check("initialize COM", r)
}

// onCOMThread runs f on a locked OS thread with COM initialized.
func onCOMThread(f func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := comInit(); err != nil {
			result <- err
			return
		}
		defer coUninitialize.Call()
		result <- f()
	}()
	return <-result
}

func newEnumerator() (comObject, error) {
	var e comObject
	r, _, _ := coCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&e)))
	return e, check("create audio device enumerator", r)
}

func deviceID(d comObject) (string, error) {
	var p uintptr
	if err := check("read device ID", d.call(deviceGetID, uintptr(unsafe.Pointer(&p)))); err != nil {
		return "", err
	}
	defer coTaskMemFree.Call(p)
	return utf16At(p), nil
}

func deviceName(d comObject) string {
	var store comObject
	if check("", d.call(deviceProperties, stgmRead, uintptr(unsafe.Pointer(&store)))) != nil {
		return ""
	}
	defer store.release()
	var v propVariant
	if check("", store.call(propertyGetValue, uintptr(unsafe.Pointer(&keyFriendlyName)), uintptr(unsafe.Pointer(&v)))) != nil {
		return ""
	}
	defer propVariantClear.Call(uintptr(unsafe.Pointer(&v)))
	if v.vt != vtLPWSTR {
		return ""
	}
	return utf16At(v.val)
}

func defaultDevice(e comObject) (comObject, error) {
	var d comObject
	err := check("find the default output device", e.call(enumDefault, eRender, eConsole, uintptr(unsafe.Pointer(&d))))
	return d, err
}

// renderEndpoints returns the active output devices in Windows' order, with
// their names. The caller releases every object.
func renderEndpoints(e comObject) ([]comObject, []Device, error) {
	var collection comObject
	if err := check("list output devices", e.call(enumEndpoints, eRender, deviceStateActive, uintptr(unsafe.Pointer(&collection)))); err != nil {
		return nil, nil, err
	}
	defer collection.release()
	var count uint32
	if err := check("count output devices", collection.call(collectionCount, uintptr(unsafe.Pointer(&count)))); err != nil {
		return nil, nil, err
	}
	objects := make([]comObject, 0, count)
	devices := make([]Device, 0, count)
	for i := uint32(0); i < count; i++ {
		var d comObject
		if check("", collection.call(collectionItem, uintptr(i), uintptr(unsafe.Pointer(&d)))) != nil {
			continue
		}
		objects = append(objects, d)
		devices = append(devices, Device{Index: int(i), Name: deviceName(d)})
	}
	return objects, devices, nil
}

func wasapiDevices() ([]Device, error) {
	var devices []Device
	err := onCOMThread(func() error {
		e, err := newEnumerator()
		if err != nil {
			return err
		}
		defer e.release()
		objects, list, err := renderEndpoints(e)
		for i := range objects {
			objects[i].release()
		}
		devices = list
		return err
	})
	return devices, err
}

const (
	commandStart = iota
	commandStop
	commandRelease
	commandClose
)

type wasapiCommand struct {
	op    int
	reply chan error
}

type wasapiStream struct {
	*softVolume
	config   OutputConfig
	callback StreamCallback
	mu       sync.Mutex // serializes the public methods
	commands chan wasapiCommand
	done     chan struct{}
	running  bool // the device thread is alive

	// Owned by the device thread.
	enumerator, device, client, render comObject
	event                              uintptr
	deviceID                           string
	keepFrames                         uint32        // frames kept queued at the device
	chunkFrames                        uint32        // output frames per player block
	latency                            time.Duration // the audio engine's own delay
	frameBytes                         int
	converter                          *converter
	playing                            bool
}

func newWASAPIStream(volume *softVolume) *wasapiStream {
	return &wasapiStream{softVolume: volume}
}

func (s *wasapiStream) Init(callback StreamCallback) error {
	s.callback = callback
	s.config = Output
	if err := s.config.Validate(); err != nil {
		return err
	}
	if s.config.BufferMs == 0 {
		s.config.BufferMs = DefaultBufferMs
	}
	s.frameBytes = 2 * s.config.Bits / 8
	s.commands = make(chan wasapiCommand)
	s.done = make(chan struct{})
	ready := make(chan error, 1)
	go s.run(ready)
	if err := <-ready; err != nil {
		<-s.done
		return err
	}
	s.running = true
	return nil
}

func (s *wasapiStream) send(op int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		if op == commandStart {
			return errors.New("WASAPI output is closed")
		}
		return nil
	}
	reply := make(chan error, 1)
	s.commands <- wasapiCommand{op, reply}
	err := <-reply
	if op == commandClose {
		<-s.done
		s.running = false
	}
	return err
}

func (s *wasapiStream) Start() error { return s.send(commandStart) }
func (s *wasapiStream) Stop() error  { return s.send(commandStop) }

// Release stops playback and closes the device until the next Start, which
// then follows the current Windows default device.
func (s *wasapiStream) Release() error { return s.send(commandRelease) }
func (s *wasapiStream) Close() error   { return s.send(commandClose) }

func (s *wasapiStream) run(ready chan<- error) {
	defer close(s.done)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if avSetMmThread.Find() == nil {
		// Let Windows schedule this thread as audio, so the short queue
		// survives a busy computer.
		task := uint32(0)
		name, _ := syscall.UTF16PtrFromString("Pro Audio")
		if h, _, _ := avSetMmThread.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&task))); h != 0 {
			defer avRevertMmThread.Call(h)
		}
	}
	if err := comInit(); err != nil {
		ready <- err
		return
	}
	defer coUninitialize.Call()
	s.event, _, _ = createEvent.Call(0, 0, 0, 0)
	if s.event == 0 {
		ready <- errors.New("cannot create Windows audio event")
		return
	}
	defer closeHandle.Call(s.event)
	e, err := newEnumerator()
	if err != nil {
		ready <- err
		return
	}
	s.enumerator = e
	defer s.enumerator.release()
	defer s.closeDevice()
	if err := s.open(); err != nil {
		ready <- err
		return
	}
	ready <- nil
	s.loop()
}

func (s *wasapiStream) loop() {
	var retry, checkDefault time.Time
	failing := false
	for {
		if !s.playing {
			if s.handle(<-s.commands) {
				return
			}
			continue
		}
		select {
		case c := <-s.commands:
			if s.handle(c) {
				return
			}
			continue
		default:
		}
		now := time.Now()
		if s.client == 0 {
			// The device went away: reopen it (or the new default device)
			// once a second until it is back.
			if !now.Before(retry) {
				if err := s.restart(); err != nil {
					if !failing {
						log.Printf("AUDIO WASAPI cannot reopen output (%v); retrying", err)
						failing = true
					}
					s.closeDevice()
					retry = now.Add(time.Second)
				} else {
					failing = false
				}
			}
			select {
			case c := <-s.commands:
				if s.handle(c) {
					return
				}
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		if s.config.Device == "" && now.After(checkDefault) {
			checkDefault = now.Add(time.Second)
			if s.defaultChanged() {
				log.Print("AUDIO Windows default output device changed; switching")
				s.closeDevice()
				retry = time.Time{}
				continue
			}
		}
		if err := s.fill(); err != nil {
			log.Printf("AUDIO WASAPI output lost (%v); reopening", err)
			s.closeDevice()
			retry = time.Time{}
			continue
		}
		waitEvent.Call(s.event, 20)
	}
}

// handle runs one command; it reports whether the thread should exit.
func (s *wasapiStream) handle(c wasapiCommand) bool {
	var err error
	exit := false
	switch c.op {
	case commandStart:
		if s.playing {
			break
		}
		if s.client == 0 {
			err = s.restart()
		} else {
			err = s.startClient()
		}
		if err != nil {
			s.closeDevice()
		} else {
			s.playing = true
		}
	case commandStop:
		s.stopClient()
	case commandRelease:
		s.stopClient()
		s.closeDevice()
	case commandClose:
		s.stopClient()
		exit = true
	}
	c.reply <- err
	return exit
}

func (s *wasapiStream) restart() error {
	if err := s.open(); err != nil {
		return err
	}
	return s.startClient()
}

// startClient fills the queue first, so playback does not begin with a
// gap, then starts the device.
func (s *wasapiStream) startClient() error {
	if err := s.fill(); err != nil {
		return err
	}
	return check("start output", s.client.call(clientStart))
}

func (s *wasapiStream) stopClient() {
	if s.playing && s.client != 0 {
		s.client.call(clientStop)
		s.client.call(clientReset) // drop queued audio, as waveOutReset does
	}
	s.playing = false
}

func (s *wasapiStream) closeDevice() {
	s.render.release()
	s.client.release()
	s.device.release()
	s.deviceID = ""
}

// findDevice returns the configured device, or the Windows default one.
func (s *wasapiStream) findDevice() (comObject, string, error) {
	if s.config.Device == "" {
		d, err := defaultDevice(s.enumerator)
		if err != nil {
			return 0, "", err
		}
		return d, deviceName(d), nil
	}
	objects, devices, err := renderEndpoints(s.enumerator)
	if err != nil {
		return 0, "", err
	}
	chosen, err := matchDevice(s.config.Device, devices)
	var found comObject
	for i := range objects {
		if err == nil && devices[i].Index == chosen.Index {
			found = objects[i]
			continue
		}
		objects[i].release()
	}
	if err != nil {
		return 0, "", err
	}
	return found, chosen.Name, nil
}

func (s *wasapiStream) defaultChanged() bool {
	d, err := defaultDevice(s.enumerator)
	if err != nil {
		return false
	}
	defer d.release()
	id, err := deviceID(d)
	return err == nil && id != s.deviceID
}

func (s *wasapiStream) open() error {
	s.closeDevice()
	device, name, err := s.findDevice()
	if err != nil {
		return err
	}
	s.device = device
	if id, err := deviceID(device); err == nil {
		s.deviceID = id
	}
	rate, bits := s.config.Rate, s.config.Bits
	format := pcmFormat(rate, bits)
	// Windows converts our format to the engine's mix format. A device
	// buffer larger than what is kept queued leaves room for a late wakeup.
	flags := uintptr(streamEventCallback | streamAutoConvertPCM | streamSRCDefaultQuality)
	requested := time.Duration(max(2*s.config.BufferMs, 50)) * time.Millisecond
	operation := fmt.Sprintf("open output %q at %d Hz %d-bit", name, rate, bits)
	for _, duration := range []time.Duration{requested, 0} {
		s.client.release()
		if err = check(operation, device.call(deviceActivate, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&s.client)))); err != nil {
			break
		}
		// Some drivers only accept the engine's default buffer size.
		if err = check(operation, s.client.call(clientInitialize, sharedMode, flags, uintptr(duration/referenceTimeUnit), 0, uintptr(unsafe.Pointer(&format)), 0)); err == nil {
			break
		}
	}
	if err == nil {
		err = check("set output event", s.client.call(clientSetEvent, s.event))
	}
	var bufferFrames uint32
	if err == nil {
		err = check("read output buffer size", s.client.call(clientBufferSize, uintptr(unsafe.Pointer(&bufferFrames))))
	}
	if err == nil {
		err = check("open output renderer", s.client.call(clientService, uintptr(unsafe.Pointer(&iidIAudioRenderClient)), uintptr(unsafe.Pointer(&s.render))))
	}
	if err != nil {
		s.closeDevice()
		return err
	}
	var latency, period, minimum int64
	s.client.call(clientLatency, uintptr(unsafe.Pointer(&latency)))
	s.client.call(clientPeriod, uintptr(unsafe.Pointer(&period)), uintptr(unsafe.Pointer(&minimum)))
	s.latency = time.Duration(latency) * referenceTimeUnit
	periodFrames := uint32(time.Duration(period) * referenceTimeUnit * time.Duration(rate) / time.Second)
	keep := uint32(s.config.BufferMs * rate / 1000)
	// Less than one engine period would run dry on every pass.
	keep = max(keep, periodFrames+uint32(rate/1000))
	s.keepFrames = min(keep, bufferFrames)
	s.chunkFrames = uint32(waveSourceFrames * rate / SourceRate)
	s.converter = newConverter(s.config, waveSourceFrames)
	ms := func(frames uint32) float64 { return float64(frames) * 1000 / float64(rate) }
	log.Printf("AUDIO output device=%q format=%d Hz %d-bit dither=%s", name, rate, bits, s.config.Dither)
	log.Printf("AUDIO WASAPI shared mode: keeping %.1f ms queued (device buffer %.1f ms, engine period %.1f ms, engine latency %.1f ms)",
		ms(s.keepFrames), ms(bufferFrames), float64(time.Duration(period)*referenceTimeUnit)/float64(time.Millisecond),
		float64(s.latency)/float64(time.Millisecond))
	return nil
}

// fill tops the device queue up to keepFrames, one player block at a time,
// telling the player how far ahead of the speaker each block is.
func (s *wasapiStream) fill() error {
	var padding uint32
	if err := check("read output queue", s.client.call(clientPadding, uintptr(unsafe.Pointer(&padding)))); err != nil {
		return err
	}
	for padding < s.keepFrames {
		n := min(s.keepFrames-padding, s.chunkFrames)
		var data uintptr
		if err := check("get output buffer", s.render.call(renderGetBuffer, uintptr(n), uintptr(unsafe.Pointer(&data)))); err != nil {
			return err
		}
		out := unsafe.Slice((*byte)(unsafe.Pointer(data)), int(n)*s.frameBytes)
		// Audio written now plays after what is queued, the engine's own
		// delay, and what the resampler still holds.
		ahead := frameTime(uint64(padding), s.config.Rate) + s.latency
		s.converter.fill(out, int(n), s.gain(), func(src []int16) {
			pending := time.Duration(s.converter.pendingSourceFrames() * float64(time.Second) / SourceRate)
			s.callback(src, 0, ahead+pending)
		})
		if err := check("write output buffer", s.render.call(renderFreeBuffer, uintptr(n), 0)); err != nil {
			return err
		}
		padding += n
	}
	return nil
}
