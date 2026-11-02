//go:build windows

package audio

import (
	"math"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"github.com/pion/rtp"
)

// fdkBuffers is AACENC_BufDesc from aacenc_lib.h.
type fdkBuffers struct {
	count    int32
	buffers  *unsafe.Pointer
	ids      *int32
	sizes    *int32
	elements *int32
}

// encodeSurround encodes a 440 Hz sine in the front left and right channels
// of a 48 kHz 5.1 stream (MODE_1_2_2_1, WAV channel order) with the shipped
// FDK library, returning raw AAC-LC access units.
func encodeSurround(t *testing.T) [][]byte {
	t.Helper()
	fdk := syscall.NewLazyDLL("libfdk-aac-2.dll")
	open, setParam, encode, closeEnc := fdk.NewProc("aacEncOpen"), fdk.NewProc("aacEncoder_SetParam"), fdk.NewProc("aacEncEncode"), fdk.NewProc("aacEncClose")
	if err := open.Find(); err != nil {
		t.Skipf("FDK AAC library not available: %v", err)
	}
	var handle uintptr
	if code, _, _ := open.Call(uintptr(unsafe.Pointer(&handle)), 0, 6); code != 0 {
		t.Fatalf("aacEncOpen: %#x", code)
	}
	defer closeEnc.Call(uintptr(unsafe.Pointer(&handle)))
	// AOT AAC-LC, bit rate, sample rate, channel mode 5.1, raw transport.
	for _, p := range [][2]uintptr{{0x100, 2}, {0x101, 320000}, {0x103, 48000}, {0x106, 6}, {0x300, 0}} {
		if code, _, _ := setParam.Call(handle, p[0], p[1]); code != 0 {
			t.Fatalf("aacEncoder_SetParam(%#x): %#x", p[0], code)
		}
	}
	if code, _, _ := encode.Call(handle, 0, 0, 0, 0); code != 0 {
		t.Fatalf("aacEncEncode init: %#x", code)
	}
	var packets [][]byte
	for frame := 0; frame < 16; frame++ {
		pcm := make([]int16, 1024*6)
		for i := 0; i < 1024; i++ {
			v := int16(12000 * math.Sin(2*math.Pi*440*float64(frame*1024+i)/48000))
			pcm[6*i], pcm[6*i+1] = v, v
		}
		out := make([]byte, 8192)
		inPtr, outPtr := unsafe.Pointer(&pcm[0]), unsafe.Pointer(&out[0])
		inID, inSize, inEl := int32(0), int32(len(pcm)*2), int32(2)
		outID, outSize, outEl := int32(3), int32(len(out)), int32(1)
		src := fdkBuffers{1, &inPtr, &inID, &inSize, &inEl}
		dst := fdkBuffers{1, &outPtr, &outID, &outSize, &outEl}
		inArgs := [2]int32{int32(len(pcm)), 0}
		var outArgs [4]int32
		code, _, _ := encode.Call(handle, uintptr(unsafe.Pointer(&src)), uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&inArgs)), uintptr(unsafe.Pointer(&outArgs)))
		runtime.KeepAlive(pcm)
		runtime.KeepAlive(out)
		if code != 0 {
			t.Fatalf("aacEncEncode: %#x", code)
		}
		if n := outArgs[0]; n > 0 {
			packets = append(packets, append([]byte(nil), out[:n]...))
		}
	}
	if len(packets) < 10 {
		t.Fatalf("encoder produced %d access units", len(packets))
	}
	return packets
}

// 5.1 AAC is decoded and downmixed to stereo; the sine in the front
// channels must come through, at the player's 44.1 kHz.
func TestBufferedDecoderDownmixesSurround(t *testing.T) {
	packets := encodeSurround(t)
	d := newBufferedDecoder(formatAAC44100, newRTPMapper(48000))
	defer d.Close()
	var peak int16
	decoded := 0
	for i, payload := range packets {
		packet := TcpPacket{Packet: rtp.Packet{Header: rtp.Header{SSRC: ssrcAAC48000x51, Timestamp: uint32(i * 1024)}}}
		frame, err := d.decode(packet, payload)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if frame == nil {
			continue
		}
		decoded++
		if !frame.aac {
			t.Fatal("decoded surround frame not marked as AAC")
		}
		for j := 0; j+1 < len(frame.pcmData); j += 2 {
			if v := int16(uint16(frame.pcmData[j]) | uint16(frame.pcmData[j+1])<<8); v > peak {
				peak = v
			}
		}
	}
	if decoded < 8 || peak < 2000 {
		t.Fatalf("decoded %d frames with peak %d; the downmix lost the front channels", decoded, peak)
	}
}
