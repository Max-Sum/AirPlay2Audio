//go:build windows

package audio

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"
)

// readAACFixture reads a fixture written by tools/generate_aac_fixtures.py:
// big-endian 16-bit lengths followed by raw AAC access units.
func readAACFixture(t *testing.T, name string) [][]byte {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", name))
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

// 5.1 and 7.1 AAC are decoded and downmixed to stereo; the sine in the
// front channels must come through, at the player's 44.1 kHz.
func TestBufferedDecoderDownmixesSurround(t *testing.T) {
	for _, test := range []struct {
		fixture string
		ssrc    uint32
	}{
		{"sine-aac-lc-5.1.bin", ssrcAAC48000x51},
		{"sine-aac-lc-7.1.bin", ssrcAAC48000x71},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			packets := readAACFixture(t, test.fixture)
			d := newBufferedDecoder(formatAAC44100, newRTPMapper(48000))
			defer d.Close()
			var peak int16
			decoded := 0
			for i, payload := range packets {
				packet := TcpPacket{Packet: rtp.Packet{Header: rtp.Header{SSRC: test.ssrc, Timestamp: uint32(i * 1024)}}}
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
		})
	}
}
