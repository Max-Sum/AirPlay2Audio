//go:build windows

package audio

import (
	"encoding/binary"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/brutella/hc/crypto/chacha20poly1305"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/ptp"
)

// Exercise actual AAC-LC decoding after authenticating an AirPlay type-103
// packet, using a generated sine wave rather than a copyrighted recording.
func TestEncryptedBufferedAACDecode(t *testing.T) {
	file, err := os.Open(filepath.Join("testdata", "sine-aac-lc.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := newBufferedDecoder(formatAAC44100)
	defer decoder.Close()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	decoded, nonzero := 0, 0
	for sequence := 0; ; sequence++ {
		var size uint16
		if err := binary.Read(file, binary.BigEndian, &size); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		encoded := make([]byte, size)
		if _, err := io.ReadFull(file, encoded); err != nil {
			t.Fatal(err)
		}
		header := make([]byte, 12)
		header[0], header[3] = 0x80, byte(sequence)
		binary.BigEndian.PutUint32(header[4:8], uint32(sequence*1024))
		binary.BigEndian.PutUint32(header[8:12], 0x12345678)
		nonce := make([]byte, 8)
		binary.BigEndian.PutUint64(nonce, uint64(sequence))
		ciphertext, tag, err := chacha20poly1305.EncryptAndSeal(key, nonce, encoded, header[4:12])
		if err != nil {
			t.Fatal(err)
		}
		packet := append(append(append(header, ciphertext...), tag[:]...), nonce...)
		if sequence == 0 {
			tampered := append([]byte(nil), packet...)
			tampered[len(tampered)-9] ^= 0x01
			if _, err := NewFrame(decoder, key, tampered); err == nil {
				t.Fatal("modified authentication tag was accepted")
			}
		}
		frame, err := NewFrame(decoder, key, packet)
		if err != nil {
			t.Fatalf("frame %d: %v", sequence, err)
		}
		if frame == nil {
			// The decoder outputs each unit one unit late.
			if sequence > 0 {
				t.Fatalf("no audio for packet %d", sequence)
			}
			continue
		}
		// Decoded audio keeps the timestamp of the packet it came from.
		if len(frame.Data()) != 2048 || frame.Timestamp != uint32(decoded*1024) {
			t.Fatalf("packet %d: invalid decoded frame (%d samples, timestamp %d, want %d)",
				sequence, len(frame.Data()), frame.Timestamp, decoded*1024)
		}
		for _, b := range frame.Data() {
			if b != 0 {
				nonzero++
				break
			}
		}
		decoded++
	}
	if decoded < 10 || nonzero < 5 {
		t.Fatalf("decoded %d packets, %d non-silent", decoded, nonzero)
	}
}

func TestAuthenticatedEmptyAccessUnitIsIgnored(t *testing.T) {
	key, nonce, header := make([]byte, 32), make([]byte, 8), make([]byte, 12)
	header[0] = 0x80
	ciphertext, tag, err := chacha20poly1305.EncryptAndSeal(key, nonce, nil, header[4:12])
	if err != nil {
		t.Fatal(err)
	}
	packet := append(append(append(header, ciphertext...), tag[:]...), nonce...)
	frame, err := NewFrame(nil, key, packet)
	if err != nil || frame != nil {
		t.Fatalf("empty unit: frame=%v error=%v", frame, err)
	}
	packet[len(packet)-9] ^= 1
	if _, err = NewFrame(nil, key, packet); err == nil {
		t.Fatal("empty unit with invalid tag was accepted")
	}
}

func TestNextStreamGetsANewDecoder(t *testing.T) {
	globals.ErrLog = log.New(io.Discard, "", 0)
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(4))
	server, err := NewServer(player)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	port, err := server.Setup(key, 0x400000)
	if err != nil {
		t.Fatal(err)
	}
	first, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	first.Write([]byte{0, 1}) // end the first audio connection with a short packet
	first.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := first.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("first connection did not close: %v", err)
	}
	first.Close()
	port, err = server.Setup(key, 0x400000)
	if err != nil {
		t.Fatal(err)
	}
	second, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	defer server.Teardown()
	file, err := os.Open(filepath.Join("testdata", "sine-aac-lc.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// Send two packets: the decoder outputs each unit one unit late.
	for sequence := 0; sequence < 2; sequence++ {
		var size uint16
		if err = binary.Read(file, binary.BigEndian, &size); err != nil {
			t.Fatal(err)
		}
		encoded := make([]byte, size)
		if _, err = io.ReadFull(file, encoded); err != nil {
			t.Fatal(err)
		}
		header := make([]byte, 12)
		header[0], header[3] = 0x80, byte(sequence)
		binary.BigEndian.PutUint32(header[4:8], uint32(sequence*1024))
		nonce := make([]byte, 8)
		binary.BigEndian.PutUint64(nonce, uint64(sequence))
		ciphertext, tag, err := chacha20poly1305.EncryptAndSeal(key, nonce, encoded, header[4:12])
		if err != nil {
			t.Fatal(err)
		}
		packet := append(append(append(header, ciphertext...), tag[:]...), nonce...)
		if err = binary.Write(second, binary.BigEndian, uint16(len(packet)+2)); err != nil {
			t.Fatal(err)
		}
		if _, err = second.Write(packet); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if frame, err := player.ringBuffer.TryPop(); err == nil {
			if len(frame.(*PCMFrame).Data()) != 2048 {
				t.Fatal("invalid second decoded frame")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("second connection never delivered a decoded frame")
}
