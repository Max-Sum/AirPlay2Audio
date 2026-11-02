//go:build windows

package codec

// This adapter calls the FDK AAC DLL directly, so the Windows build does not
// need a C compiler. The original decoder adapter and its license are in aac.go.
import (
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	fdk       = syscall.NewLazyDLL("libfdk-aac-2.dll")
	fdkOpen   = fdk.NewProc("aacDecoder_Open")
	fdkConfig = fdk.NewProc("aacDecoder_ConfigRaw")
	fdkFill   = fdk.NewProc("aacDecoder_Fill")
	fdkDecode = fdk.NewProc("aacDecoder_DecodeFrame")
	fdkInfo   = fdk.NewProc("aacDecoder_GetStreamInfo")
	fdkClose  = fdk.NewProc("aacDecoder_Close")
	fdkParam  = fdk.NewProc("aacDecoder_SetParam")
)

// AAC_PCM_MAX_OUTPUT_CHANNELS in aacdecoder_lib.h.
const fdkMaxOutputChannels = 0x0012

// SetStereoOutput makes the decoder downmix multichannel audio (5.1, 7.1)
// to stereo.
func (d *AacDecoder) SetStereoOutput() error {
	if d.handle == 0 {
		return fmt.Errorf("AAC decoder not initialized")
	}
	if err := fdkParam.Find(); err != nil {
		return err
	}
	if code, _, _ := fdkParam.Call(d.handle, fdkMaxOutputChannels, 2); code != 0 {
		return fmt.Errorf("AAC downmix setting failed: %#x", code)
	}
	return nil
}

type AacDecoder struct{ handle uintptr }
type fdkStreamInfo struct{ SampleRate, FrameSize, Channels int32 }

func NewAacDecoder() *AacDecoder { return &AacDecoder{} }

func (d *AacDecoder) InitRaw(asc []byte) error {
	if len(asc) == 0 {
		return fmt.Errorf("empty AAC configuration")
	}
	if err := fdkOpen.Find(); err != nil {
		return fmt.Errorf("load AAC decoder: %w", err)
	}
	d.handle, _, _ = fdkOpen.Call(0, 1) // TT_MP4_RAW, one layer
	if d.handle == 0 {
		return fmt.Errorf("cannot open AAC decoder")
	}
	ptr := uintptr(unsafe.Pointer(&asc[0]))
	size := uint32(len(asc))
	code, _, _ := fdkConfig.Call(d.handle, uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&size)))
	runtime.KeepAlive(asc)
	if code != 0 {
		d.Close()
		return fmt.Errorf("AAC configuration failed: %#x", code)
	}
	return nil
}

func (d *AacDecoder) Close() error {
	if d.handle != 0 {
		fdkClose.Call(d.handle)
		d.handle = 0
	}
	return nil
}

func (d *AacDecoder) Decode(frame []byte) ([]byte, error) {
	if d.handle == 0 || len(frame) == 0 {
		return nil, fmt.Errorf("empty AAC frame or decoder not initialized")
	}
	ptr := uintptr(unsafe.Pointer(&frame[0]))
	size := uint32(len(frame))
	remaining := size
	code, _, _ := fdkFill.Call(d.handle, uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&remaining)))
	runtime.KeepAlive(frame)
	if code != 0 || remaining != 0 {
		return nil, fmt.Errorf("AAC input failed: %#x, %d bytes left", code, remaining)
	}
	pcm := make([]int16, 8192)
	code, _, _ = fdkDecode.Call(d.handle, uintptr(unsafe.Pointer(&pcm[0])), uintptr(len(pcm)), 0)
	if code == 0x1002 {
		return nil, nil
	}
	if code != 0 {
		return nil, fmt.Errorf("AAC decode failed: %#x", code)
	}
	infoPtr, _, _ := fdkInfo.Call(d.handle)
	if infoPtr == 0 {
		return nil, fmt.Errorf("AAC decoder returned no stream information")
	}
	info := (*fdkStreamInfo)(unsafe.Pointer(infoPtr))
	if info.SampleRate != 44100 && info.SampleRate != 48000 || info.Channels != 2 || info.FrameSize <= 0 || int(info.FrameSize*info.Channels) > len(pcm) {
		return nil, fmt.Errorf("unsupported AAC output: %d Hz, %d channels, %d samples", info.SampleRate, info.Channels, info.FrameSize)
	}
	count := int(info.FrameSize * info.Channels)
	out := make([]byte, count*2)
	for i := 0; i < count; i++ {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(pcm[i]))
	}
	return out, nil
}
