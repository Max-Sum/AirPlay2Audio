package event

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/brutella/hc/crypto/chacha20poly1305"
	"github.com/brutella/hc/crypto/hkdf"
)

// writeFrames encrypts data the way an AirPlay sender writes the event channel.
func writeFrames(writer io.Writer, key [32]byte, data []byte) error {
	var counter uint64
	for len(data) > 0 {
		n := len(data)
		if n > 1024 {
			n = 1024
		}
		var length [2]byte
		binary.LittleEndian.PutUint16(length[:], uint16(n))
		var nonce [8]byte
		binary.LittleEndian.PutUint64(nonce[:], counter)
		counter++
		ciphertext, tag, err := chacha20poly1305.EncryptAndSeal(key[:], nonce[:], data[:n], length[:])
		if err != nil {
			return err
		}
		if _, err = writer.Write(append(append(length[:], ciphertext...), tag[:]...)); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func TestEventReaderDecryptsSenderFrames(t *testing.T) {
	secret := make([]byte, 64) // transient pairing uses the 64-byte SRP key
	for i := range secret {
		secret[i] = byte(i)
	}
	senderKey, err := hkdf.Sha512(secret, []byte("Events-Salt"), []byte("Events-Read-Encryption-Key"))
	if err != nil {
		t.Fatal(err)
	}
	if senderKey != incomingKey(secret) {
		t.Fatal("incoming key does not match the sender's write key")
	}
	message := append([]byte("POST /command RTSP/1.0\r\nContent-Length: 1500\r\n\r\n"), bytes.Repeat([]byte("x"), 1500)...)
	var wire bytes.Buffer
	if err := writeFrames(&wire, senderKey, message); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(&eventReader{reader: &wire, key: incomingKey(secret)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, message) {
		t.Fatal("decrypted event data differs")
	}
}
