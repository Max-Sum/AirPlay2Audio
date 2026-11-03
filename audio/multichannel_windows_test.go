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

// 5.1 AAC is decoded and downmixed to stereo; the sine in the front
// channels must come through, at the player's 44.1 kHz.
func TestBufferedDecoderDownmixesSurround(t *testing.T) {
	for _, test := range []struct {
		fixture string
		ssrc    uint32
	}{
		{"sine-aac-lc-5.1.bin", ssrcAAC48000x51},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			packets := readAACFixture(t, test.fixture)
			d := newBufferedDecoder(formatAAC44100)
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
				for _, s := range frame.samples {
					if v := int16(s >> 16); v > peak {
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

// The Windows AAC decoder cannot decode 7.1: it is not advertised, and its
// packets are skipped without ending the stream.
func TestBuffered71IsSkippedOnWindows(t *testing.T) {
	if SupportedBufferedFormat(audioFormatAAC48000x71) {
		t.Fatal("7.1 advertised")
	}
	d := newBufferedDecoder(formatAAC44100)
	defer d.Close()
	for i, payload := range readAACFixture(t, "sine-aac-lc-7.1.bin") {
		packet := TcpPacket{Packet: rtp.Packet{Header: rtp.Header{SSRC: ssrcAAC48000x71, Timestamp: uint32(i * 1024)}}}
		if frame, err := d.decode(packet, payload); err != nil || frame != nil {
			t.Fatalf("packet %d: frame=%v error=%v", i, frame, err)
		}
	}
}
