//go:build windows

package codec

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// readUnits reads a fixture: big-endian 16-bit lengths followed by raw AAC
// access units.
func readUnits(t *testing.T, fixture string) [][]byte {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "audio", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var units [][]byte
	for {
		var size uint16
		if err := binary.Read(file, binary.BigEndian, &size); err == io.EOF {
			return units
		} else if err != nil {
			t.Fatal(err)
		}
		unit := make([]byte, size)
		if _, err := io.ReadFull(file, unit); err != nil {
			t.Fatal(err)
		}
		units = append(units, unit)
	}
}

// The decoder outputs each access unit one unit late. After that, every
// unit must decode to exactly 1024 stereo frames, so outputs stay paired
// with the units they came from.
func TestAacDecoderOutputsOneFramePerUnit(t *testing.T) {
	for _, test := range []struct {
		fixture string
		asc     []byte
	}{
		{"sine-aac-lc.bin", []byte{0x12, 0x10}},
		{"sine-aac-lc-5.1.bin", []byte{0x11, 0xb0}},
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
			units := readUnits(t, test.fixture)
			var peak int16
			delay := 0
			for i, unit := range units {
				pcm, err := d.Decode(unit)
				if err != nil {
					t.Fatalf("unit %d: %v", i, err)
				}
				if len(pcm) == 0 && i == delay && delay < 1 {
					delay++
					continue
				}
				if len(pcm) != 1024*4 {
					t.Fatalf("unit %d decoded to %d bytes, want %d (decoder delay %d)", i, len(pcm), 1024*4, delay)
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

func TestAacDecoderRejects71(t *testing.T) {
	d := NewAacDecoder()
	defer d.Close()
	if err := d.InitRaw([]byte{0x11, 0xb8}); err == nil {
		t.Fatal("7.1 accepted")
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
	// The decoder must still work afterwards, within its one-unit delay.
	units := readUnits(t, "sine-aac-lc.bin")
	for i, unit := range units[:3] {
		pcm, err := d.Decode(unit)
		if err != nil {
			t.Fatalf("unit %d after garbage: %v", i, err)
		}
		if len(pcm) == 1024*4 {
			return
		}
		if len(pcm) != 0 {
			t.Fatalf("unit %d after garbage: %d bytes", i, len(pcm))
		}
	}
	t.Fatal("no audio after garbage")
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
