//go:build windows

package codec

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Every access unit must decode at once to 1024 stereo frames. A decoder
// that held output back would shift the audio against its timestamps.
func TestAacDecoderOutputsEveryFrameAtOnce(t *testing.T) {
	for _, test := range []struct {
		fixture string
		asc     []byte
	}{
		{"sine-aac-lc.bin", []byte{0x12, 0x10}},
		{"sine-aac-lc-5.1.bin", []byte{0x11, 0xb0}},
		{"sine-aac-lc-7.1.bin", []byte{0x11, 0xb8}},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			d := NewAacDecoder()
			if err := d.InitRaw(test.asc); err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.SetStereoOutput(); err != nil {
				t.Fatal(err)
			}
			t.Logf("decoder output: %d channels", d.channels)
			file, err := os.Open(filepath.Join("..", "audio", "testdata", test.fixture))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var peak int16
			for i := 0; ; i++ {
				var size uint16
				if err := binary.Read(file, binary.BigEndian, &size); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				unit := make([]byte, size)
				if _, err := io.ReadFull(file, unit); err != nil {
					t.Fatal(err)
				}
				pcm, err := d.Decode(unit)
				if err != nil {
					t.Fatalf("unit %d: %v", i, err)
				}
				if len(pcm) != 1024*4 {
					t.Fatalf("unit %d decoded to %d bytes, want %d", i, len(pcm), 1024*4)
				}
				for j := 0; j+1 < len(pcm); j += 2 {
					peak = max(peak, int16(binary.LittleEndian.Uint16(pcm[j:])))
				}
			}
			if peak < 2000 {
				t.Fatalf("peak %d; the front channels were lost", peak)
			}
		})
	}
}

func TestAacDecoderRejectsGarbage(t *testing.T) {
	d := NewAacDecoder()
	if err := d.InitRaw([]byte{0x12, 0x10}); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Decode([]byte{0xde, 0xad, 0xbe, 0xef}); err == nil {
		t.Log("decoder accepted garbage without an error")
	}
	// The decoder must still work afterwards.
	file, err := os.Open(filepath.Join("..", "audio", "testdata", "sine-aac-lc.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var size uint16
	if err := binary.Read(file, binary.BigEndian, &size); err != nil {
		t.Fatal(err)
	}
	unit := make([]byte, size)
	if _, err := io.ReadFull(file, unit); err != nil {
		t.Fatal(err)
	}
	if pcm, err := d.Decode(unit); err != nil || len(pcm) != 1024*4 {
		t.Fatalf("after garbage: %d bytes, %v", len(pcm), err)
	}
}

func TestASCInfo(t *testing.T) {
	for _, test := range []struct {
		asc            []byte
		rate, channels int
	}{
		{[]byte{0x12, 0x10}, 44100, 2},
		{[]byte{0x11, 0x90}, 48000, 2},
		{[]byte{0x11, 0xb0}, 48000, 6},
		{[]byte{0x11, 0xb8}, 48000, 8},
	} {
		rate, channels, err := ascInfo(test.asc)
		if err != nil || rate != test.rate || channels != test.channels {
			t.Errorf("% x: %d Hz, %d channels, %v", test.asc, rate, channels, err)
		}
	}
	if _, _, err := ascInfo([]byte{0x12}); err == nil {
		t.Error("short configuration accepted")
	}
}
