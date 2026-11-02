//go:build windows

package codec

// The Windows build decodes AAC with the Media Foundation AAC decoder that
// ships with Windows, called through COM without cgo. Windows N and KN
// editions include it only after the Media Feature Pack is installed.
// COM arguments are passed as uintptr, which assumes a 64-bit build.

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

type guid struct {
	Data1        uint32
	Data2, Data3 uint16
	Data4        [8]byte
}

// GUIDs from wmcodecdsp.idl, mftransform.idl and mfapi.h.
var (
	clsidMSAACDecMFT = guid{0x32d186a7, 0x218f, 0x4c75, [8]byte{0x88, 0x76, 0xdd, 0x77, 0x27, 0x3a, 0x89, 0x99}}
	iidIMFTransform  = guid{0xbf94c121, 0x5b05, 0x4e6f, [8]byte{0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d}}

	mfMediaTypeAudio = guid{0x73647561, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
	mfAudioFormatAAC = guid{0x00001610, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
	mfAudioFormatPCM = guid{0x00000001, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}

	mfMTMajorType           = guid{0x48eba18e, 0xf8c9, 0x4687, [8]byte{0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f}}
	mfMTSubtype             = guid{0xf7e34c9a, 0x42e8, 0x4714, [8]byte{0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5}}
	mfMTAudioNumChannels    = guid{0x37e48bf5, 0x645e, 0x4c5b, [8]byte{0x89, 0xde, 0xad, 0xa9, 0xe2, 0x9b, 0x69, 0x6a}}
	mfMTAudioSamplesPerSec  = guid{0x5faeeae7, 0x0290, 0x4c31, [8]byte{0x9e, 0x8a, 0xc5, 0x34, 0xf6, 0x8d, 0x9d, 0xba}}
	mfMTAudioBitsPerSample  = guid{0xf2deb57f, 0x40fa, 0x4764, [8]byte{0xaa, 0x33, 0xed, 0x4f, 0x2d, 0x1f, 0xf6, 0x69}}
	mfMTAudioBlockAlignment = guid{0x322de230, 0x9eeb, 0x43bd, [8]byte{0xab, 0x7a, 0xff, 0x41, 0x22, 0x51, 0x54, 0x1d}}
	mfMTAudioAvgBytesPerSec = guid{0x1aab75c8, 0xcfef, 0x451c, [8]byte{0xab, 0x95, 0xac, 0x03, 0x4b, 0x8e, 0x17, 0x31}}
	mfMTAACPayloadType      = guid{0xbfbabe79, 0x7434, 0x4d1c, [8]byte{0x94, 0xf0, 0x72, 0xa3, 0xb9, 0xe1, 0x71, 0x88}}
	mfMTAACProfileLevel     = guid{0x7632f0e6, 0x9538, 0x4d61, [8]byte{0xac, 0xda, 0xea, 0x29, 0xc8, 0xc1, 0x44, 0x56}}
	mfMTUserData            = guid{0xb6bc765f, 0x4c3b, 0x40a4, [8]byte{0xbd, 0x51, 0x25, 0x35, 0xb6, 0x6f, 0xe0, 0x9d}}
)

const (
	mfVersion       = 0x00020070 // MF_SDK_VERSION << 16 | MF_API_VERSION
	mfStartupLite   = 1
	clsctxInprocSrv = 1

	mftMessageCommandFlush          = 0x00000000
	mftMessageNotifyBeginStreaming  = 0x10000000
	mftMessageNotifyStartOfStream   = 0x10000003
	mftOutputStreamProvidesSamples  = 0x00000100
	mftOutputStreamCanProvideSample = 0x00000200

	mfENotAccepting            = 0xc00d36b5
	mfENoMoreTypes             = 0xc00d36b9
	mfETransformStreamChange   = 0xc00d6d61
	mfETransformNeedMoreInput  = 0xc00d6d72
	regdbEClassNotRegistered   = 0x80040154
	maxSamplesPerChannelPerAAC = 2048 // HE-AAC frames; AAC-LC has 1024
)

// COM vtable slots.
const (
	slotRelease = 2

	// IMFAttributes
	slotGetUINT32 = 7
	slotGetGUID   = 10
	slotSetUINT32 = 21
	slotSetGUID   = 24
	slotSetBlob   = 26

	// IMFSample
	slotSetSampleTime             = 36
	slotSetSampleDuration         = 38
	slotConvertToContiguousBuffer = 41
	slotAddBuffer                 = 42
	slotRemoveAllBuffers          = 44

	// IMFMediaBuffer
	slotLock             = 3
	slotUnlock           = 4
	slotSetCurrentLength = 6

	// IMFTransform
	slotGetOutputStreamInfo    = 7
	slotGetOutputAvailableType = 14
	slotSetInputType           = 15
	slotSetOutputType          = 16
	slotProcessMessage         = 23
	slotProcessInput           = 24
	slotProcessOutput          = 25
)

var (
	ole32                = syscall.NewLazyDLL("ole32.dll")
	coIncrementMTAUsage  = ole32.NewProc("CoIncrementMTAUsage")
	coCreateInstance     = ole32.NewProc("CoCreateInstance")
	mfplat               = syscall.NewLazyDLL("mfplat.dll")
	mfStartup            = mfplat.NewProc("MFStartup")
	mfCreateMediaType    = mfplat.NewProc("MFCreateMediaType")
	mfCreateSample       = mfplat.NewProc("MFCreateSample")
	mfCreateMemoryBuffer = mfplat.NewProc("MFCreateMemoryBuffer")
	mediaFoundationOnce  sync.Once
	mediaFoundationErr   error
)

type hresult uint32

func (hr hresult) failed() bool { return int32(hr) < 0 }

func (hr hresult) Error() string {
	switch uint32(hr) {
	case regdbEClassNotRegistered:
		return "the Windows AAC decoder is not installed (on Windows N and KN editions, install the Media Feature Pack)"
	}
	return fmt.Sprintf("HRESULT %#08x", uint32(hr))
}

func check(what string, hr uintptr) error {
	if h := hresult(hr); h.failed() {
		return fmt.Errorf("%s: %w", what, h)
	}
	return nil
}

// comCall calls method slot of the COM object obj.
func comCall(obj uintptr, slot int, args ...uintptr) uintptr {
	vtable := *(*uintptr)(unsafe.Pointer(obj))
	method := *(*uintptr)(unsafe.Pointer(vtable + uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	hr, _, _ := syscall.SyscallN(method, append([]uintptr{obj}, args...)...)
	return hr
}

func release(obj uintptr) {
	if obj != 0 {
		comCall(obj, slotRelease)
	}
}

// startMediaFoundation keeps the process multithreaded apartment alive, so
// COM objects work from any goroutine, and starts Media Foundation.
func startMediaFoundation() error {
	mediaFoundationOnce.Do(func() {
		if err := coIncrementMTAUsage.Find(); err != nil {
			mediaFoundationErr = fmt.Errorf("COM is not available: %w", err)
			return
		}
		var cookie uintptr
		hr, _, _ := coIncrementMTAUsage.Call(uintptr(unsafe.Pointer(&cookie)))
		if mediaFoundationErr = check("initialize COM", hr); mediaFoundationErr != nil {
			return
		}
		if err := mfStartup.Find(); err != nil {
			mediaFoundationErr = fmt.Errorf("Media Foundation is not available (on Windows N and KN editions, install the Media Feature Pack): %w", err)
			return
		}
		hr, _, _ = mfStartup.Call(mfVersion, mfStartupLite)
		mediaFoundationErr = check("start Media Foundation", hr)
	})
	return mediaFoundationErr
}

// mfOutputDataBuffer is MFT_OUTPUT_DATA_BUFFER.
type mfOutputDataBuffer struct {
	StreamID uint32
	Sample   uintptr
	Status   uint32
	Events   uintptr
}

// AacDecoder decodes raw AAC access units to interleaved 16-bit stereo PCM.
// Multichannel streams are always downmixed to stereo. The decoder accepts
// up to 5.1; it rejects 7.1.
//
// The decoder outputs each access unit one unit late: Decode returns nothing
// for the first unit, then the previous unit's audio. Callers that track
// timestamps must pair each output with the unit it came from.
type AacDecoder struct {
	mft         uintptr
	rate        int
	channels    int // decoder output channels before any downmix
	outSize     uint32
	provides    bool    // the decoder allocates output samples
	outSample   uintptr // reused output sample when it does not
	time        int64   // next input sample time in 100 ns units
	frameLength int64   // nominal access unit duration in 100 ns units
}

func NewAacDecoder() *AacDecoder { return &AacDecoder{} }

// SetStereoOutput is kept for the shared decoder interface. The Windows
// decoder always produces stereo.
func (d *AacDecoder) SetStereoOutput() error {
	if d.mft == 0 {
		return fmt.Errorf("AAC decoder not initialized")
	}
	return nil
}

// ascInfo returns the sample rate and channel count of an AAC-LC
// AudioSpecificConfig.
func ascInfo(asc []byte) (rate, channels int, err error) {
	rates := []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
	if len(asc) < 2 {
		return 0, 0, fmt.Errorf("AAC configuration too short")
	}
	index := int(asc[0]&0x07)<<1 | int(asc[1]>>7)
	config := int(asc[1]>>3) & 0x0f
	if index >= len(rates) {
		return 0, 0, fmt.Errorf("unsupported AAC sampling frequency index %d", index)
	}
	switch {
	case config >= 1 && config <= 6:
		channels = config
	case config == 7:
		channels = 8
	default:
		return 0, 0, fmt.Errorf("unsupported AAC channel configuration %d", config)
	}
	return rates[index], channels, nil
}

func (d *AacDecoder) InitRaw(asc []byte) error {
	if len(asc) == 0 {
		return fmt.Errorf("empty AAC configuration")
	}
	rate, channels, err := ascInfo(asc)
	if err != nil {
		return err
	}
	if channels > 6 {
		return fmt.Errorf("the Windows AAC decoder does not support %d-channel audio", channels)
	}
	if err := startMediaFoundation(); err != nil {
		return err
	}
	d.Close()
	hr, _, _ := coCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMSAACDecMFT)), 0, clsctxInprocSrv,
		uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&d.mft)))
	if err := check("open AAC decoder", hr); err != nil {
		d.mft = 0
		return err
	}
	d.rate, d.time, d.frameLength = rate, 0, 1024*10_000_000/int64(rate)
	if err := d.setup(asc, channels); err != nil {
		d.Close()
		return err
	}
	return nil
}

func (d *AacDecoder) setup(asc []byte, channels int) error {
	if err := d.setInputType(asc, channels); err != nil {
		return err
	}
	if err := d.selectOutputType(); err != nil {
		return err
	}
	if err := check("start AAC decoder", comCall(d.mft, slotProcessMessage, mftMessageNotifyBeginStreaming, 0)); err != nil {
		return err
	}
	return check("start AAC decoder", comCall(d.mft, slotProcessMessage, mftMessageNotifyStartOfStream, 0))
}

func newMediaType() (uintptr, error) {
	var mediaType uintptr
	hr, _, _ := mfCreateMediaType.Call(uintptr(unsafe.Pointer(&mediaType)))
	return mediaType, check("create media type", hr)
}

func setGUID(obj uintptr, key, value *guid) error {
	return check("set media type", comCall(obj, slotSetGUID, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(value))))
}

func setUINT32(obj uintptr, key *guid, value uint32) error {
	return check("set media type", comCall(obj, slotSetUINT32, uintptr(unsafe.Pointer(key)), uintptr(value)))
}

func getUINT32(obj uintptr, key *guid) (uint32, bool) {
	var value uint32
	hr := comCall(obj, slotGetUINT32, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&value)))
	return value, !hresult(hr).failed()
}

// setInputType describes raw AAC with its AudioSpecificConfig. Some decoder
// versions reject some profile-level values, so the unspecified value 0xFE
// is tried as well.
func (d *AacDecoder) setInputType(asc []byte, channels int) error {
	level := uint32(0x29) // AAC profile, level 2: stereo up to 48 kHz
	if channels > 2 {
		level = 0x2a // level 4: multichannel up to 48 kHz
	}
	var last error
	for _, level := range []uint32{level, 0xfe} {
		last = d.trySetInputType(asc, channels, level)
		if last == nil {
			return nil
		}
	}
	return last
}

func (d *AacDecoder) trySetInputType(asc []byte, channels int, level uint32) error {
	mediaType, err := newMediaType()
	if err != nil {
		return err
	}
	defer release(mediaType)
	// MF_MT_USER_DATA is the part of HEAACWAVEINFO after WAVEFORMATEX
	// (payload type 0 = raw, profile level, struct type, reserved fields),
	// then the AudioSpecificConfig.
	userData := make([]byte, 12, 12+len(asc))
	userData[2], userData[3] = byte(level), byte(level>>8)
	userData = append(userData, asc...)
	steps := []error{
		setGUID(mediaType, &mfMTMajorType, &mfMediaTypeAudio),
		setGUID(mediaType, &mfMTSubtype, &mfAudioFormatAAC),
		setUINT32(mediaType, &mfMTAudioSamplesPerSec, uint32(d.rate)),
		setUINT32(mediaType, &mfMTAudioNumChannels, uint32(channels)),
		setUINT32(mediaType, &mfMTAACPayloadType, 0),
		setUINT32(mediaType, &mfMTAACProfileLevel, level),
		check("set media type", comCall(mediaType, slotSetBlob, uintptr(unsafe.Pointer(&mfMTUserData)),
			uintptr(unsafe.Pointer(&userData[0])), uintptr(len(userData)))),
	}
	runtime.KeepAlive(userData)
	if err := errors.Join(steps...); err != nil {
		return err
	}
	return check(fmt.Sprintf("AAC input type (%d Hz, %d channels)", d.rate, channels), comCall(d.mft, slotSetInputType, 0, mediaType, 0))
}

// selectOutputType picks 16-bit PCM, stereo when the decoder offers it;
// otherwise Decode downmixes the decoder's own channel layout.
func (d *AacDecoder) selectOutputType() error {
	chosen, chosenChannels := uintptr(0), uint32(0)
	for index := uintptr(0); ; index++ {
		var mediaType uintptr
		hr := comCall(d.mft, slotGetOutputAvailableType, 0, index, uintptr(unsafe.Pointer(&mediaType)))
		if uint32(hr) == mfENoMoreTypes {
			break
		}
		if err := check("list AAC output types", hr); err != nil {
			release(chosen)
			return err
		}
		var subtype guid
		comCall(mediaType, slotGetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&subtype)))
		bits, _ := getUINT32(mediaType, &mfMTAudioBitsPerSample)
		channels, _ := getUINT32(mediaType, &mfMTAudioNumChannels)
		rate, _ := getUINT32(mediaType, &mfMTAudioSamplesPerSec)
		usable := subtype == mfAudioFormatPCM && bits == 16 && channels >= 2 && int(rate) == d.rate
		if usable && (chosen == 0 || channels == 2 && chosenChannels != 2) {
			release(chosen)
			chosen, chosenChannels = mediaType, channels
			continue
		}
		release(mediaType)
	}
	if chosen == 0 {
		return fmt.Errorf("the AAC decoder offers no 16-bit PCM output at %d Hz", d.rate)
	}
	if chosenChannels != 2 {
		// The decoder did not list stereo; it may still accept a stereo
		// downmix when asked for one.
		if stereo, err := d.stereoOutputType(); err == nil {
			if !hresult(comCall(d.mft, slotSetOutputType, 0, stereo, 0)).failed() {
				release(stereo)
				release(chosen)
				d.channels = 2
				return d.readOutputInfo()
			}
			release(stereo)
		}
	}
	defer release(chosen)
	if err := check("set AAC output type", comCall(d.mft, slotSetOutputType, 0, chosen, 0)); err != nil {
		return err
	}
	d.channels = int(chosenChannels)
	return d.readOutputInfo()
}

func (d *AacDecoder) stereoOutputType() (uintptr, error) {
	mediaType, err := newMediaType()
	if err != nil {
		return 0, err
	}
	err = errors.Join(
		setGUID(mediaType, &mfMTMajorType, &mfMediaTypeAudio),
		setGUID(mediaType, &mfMTSubtype, &mfAudioFormatPCM),
		setUINT32(mediaType, &mfMTAudioSamplesPerSec, uint32(d.rate)),
		setUINT32(mediaType, &mfMTAudioNumChannels, 2),
		setUINT32(mediaType, &mfMTAudioBitsPerSample, 16),
		setUINT32(mediaType, &mfMTAudioBlockAlignment, 4),
		setUINT32(mediaType, &mfMTAudioAvgBytesPerSec, uint32(d.rate*4)),
	)
	if err != nil {
		release(mediaType)
		return 0, err
	}
	return mediaType, nil
}

// readOutputInfo learns who allocates output samples and how large they
// must be, and prepares a reusable output sample if needed.
func (d *AacDecoder) readOutputInfo() error {
	var info struct{ Flags, Size, Alignment uint32 }
	if err := check("read AAC output info", comCall(d.mft, slotGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&info)))); err != nil {
		return err
	}
	d.provides = info.Flags&(mftOutputStreamProvidesSamples|mftOutputStreamCanProvideSample) != 0
	d.outSize = max(info.Size, uint32(maxSamplesPerChannelPerAAC*d.channels*2))
	release(d.outSample)
	d.outSample = 0
	if d.provides {
		return nil
	}
	sample, err := newSample(d.outSize)
	if err != nil {
		return err
	}
	d.outSample = sample
	return nil
}

// newSample creates a sample holding one memory buffer of the given size.
func newSample(size uint32) (uintptr, error) {
	var sample, buffer uintptr
	hr, _, _ := mfCreateSample.Call(uintptr(unsafe.Pointer(&sample)))
	if err := check("create sample", hr); err != nil {
		return 0, err
	}
	hr, _, _ = mfCreateMemoryBuffer.Call(uintptr(size), uintptr(unsafe.Pointer(&buffer)))
	if err := check("create buffer", hr); err != nil {
		release(sample)
		return 0, err
	}
	defer release(buffer)
	if err := check("add buffer", comCall(sample, slotAddBuffer, buffer)); err != nil {
		release(sample)
		return 0, err
	}
	return sample, nil
}

// withBuffer runs f on the contiguous memory of a sample.
func withBuffer(sample uintptr, f func(buffer uintptr, data []byte)) error {
	var buffer uintptr
	if err := check("read sample", comCall(sample, slotConvertToContiguousBuffer, uintptr(unsafe.Pointer(&buffer)))); err != nil {
		return err
	}
	defer release(buffer)
	var data *byte
	var maxLength, length uint32
	if err := check("lock buffer", comCall(buffer, slotLock, uintptr(unsafe.Pointer(&data)),
		uintptr(unsafe.Pointer(&maxLength)), uintptr(unsafe.Pointer(&length)))); err != nil {
		return err
	}
	defer comCall(buffer, slotUnlock)
	f(buffer, unsafe.Slice(data, maxLength)[:length:maxLength])
	return nil
}

func (d *AacDecoder) Close() error {
	release(d.outSample)
	d.outSample = 0
	release(d.mft)
	d.mft = 0
	return nil
}

func (d *AacDecoder) Decode(frame []byte) ([]byte, error) {
	if d.mft == 0 || len(frame) == 0 {
		return nil, fmt.Errorf("empty AAC frame or decoder not initialized")
	}
	input, err := newSample(uint32(len(frame)))
	if err != nil {
		return nil, err
	}
	defer release(input)
	err = withBuffer(input, func(buffer uintptr, data []byte) {
		copy(data[:len(frame)], frame)
		comCall(buffer, slotSetCurrentLength, uintptr(len(frame)))
	})
	if err != nil {
		return nil, err
	}
	comCall(input, slotSetSampleTime, uintptr(d.time))
	comCall(input, slotSetSampleDuration, uintptr(d.frameLength))
	d.time += d.frameLength

	var pcm []byte
	hr := comCall(d.mft, slotProcessInput, 0, input, 0)
	if uint32(hr) == mfENotAccepting {
		// Output is pending: collect it, then the input fits.
		if pcm, err = d.drain(pcm); err != nil {
			return nil, d.reset(err)
		}
		hr = comCall(d.mft, slotProcessInput, 0, input, 0)
	}
	if err := check("AAC input", hr); err != nil {
		return nil, d.reset(err)
	}
	if pcm, err = d.drain(pcm); err != nil {
		return nil, d.reset(err)
	}
	if len(pcm) == 0 {
		return nil, nil
	}
	return downmixToStereo(pcm, d.channels), nil
}

// reset flushes the decoder after an error, so a bad access unit does not
// affect the next ones.
func (d *AacDecoder) reset(err error) error {
	comCall(d.mft, slotProcessMessage, mftMessageCommandFlush, 0)
	return fmt.Errorf("AAC decode failed: %w", err)
}

// drain appends all available output to pcm.
func (d *AacDecoder) drain(pcm []byte) ([]byte, error) {
	for {
		out := mfOutputDataBuffer{Sample: d.outSample}
		if !d.provides {
			if err := withBuffer(d.outSample, func(buffer uintptr, _ []byte) { comCall(buffer, slotSetCurrentLength, 0) }); err != nil {
				return pcm, err
			}
		}
		var status uint32
		hr := comCall(d.mft, slotProcessOutput, 0, 1, uintptr(unsafe.Pointer(&out)), uintptr(unsafe.Pointer(&status)))
		release(out.Events)
		switch uint32(hr) {
		case mfETransformNeedMoreInput:
			return pcm, nil
		case mfETransformStreamChange:
			if err := d.selectOutputType(); err != nil {
				return pcm, err
			}
			continue
		}
		if err := check("AAC output", hr); err != nil {
			return pcm, err
		}
		err := withBuffer(out.Sample, func(_ uintptr, data []byte) { pcm = append(pcm, data...) })
		if d.provides {
			release(out.Sample)
		}
		if err != nil {
			return pcm, err
		}
	}
}
