package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/brutella/hc/crypto/chacha20poly1305"
	"github.com/brutella/hc/crypto/hkdf"
	"github.com/brutella/hc/hap/pair"
	"github.com/brutella/hc/util"
)

// AirPlay 2 "transient" pairing (feature bit 48), as used by shairport-sync
// and HomePods for senders that are not HomeKit owners: only SRP steps M1-M4
// with the fixed PIN 3939, then the SRP session key becomes the shared secret
// for the control and event channels. No pair-verify follows.

const (
	tagFlags           = 0x13
	pairFlagTransient  = 0x10
	transientPin       = "3939"
	errAuthentication  = 0x02
	controlPacketLimit = 1024
)

func isTransientStart(in util.Container) bool {
	if in.GetByte(pair.TagSequence) != 1 {
		return false
	}
	flags := in.GetBytes(tagFlags)
	for _, b := range flags {
		if b&pairFlagTransient != 0 {
			return true
		}
	}
	return false
}

func transientError(state byte) util.Container {
	out := util.NewTLV8Container()
	out.SetByte(pair.TagSequence, state)
	out.SetByte(pair.TagErrCode, errAuthentication)
	return out
}

// controlSession implements crypto.Cryptographer for a secret of any length
// (the 64-byte SRP session key for transient pairing).
type controlSession struct {
	encryptKey, decryptKey     [32]byte
	encryptCount, decryptCount uint64
}

func newControlSession(secret []byte) (*controlSession, error) {
	salt := []byte("Control-Salt")
	s := &controlSession{}
	var err error
	if s.encryptKey, err = hkdf.Sha512(secret, salt, []byte("Control-Read-Encryption-Key")); err != nil {
		return nil, err
	}
	if s.decryptKey, err = hkdf.Sha512(secret, salt, []byte("Control-Write-Encryption-Key")); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *controlSession) Encrypt(r io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for len(data) > 0 {
		n := len(data)
		if n > controlPacketLimit {
			n = controlPacketLimit
		}
		var nonce [8]byte
		binary.LittleEndian.PutUint64(nonce[:], s.encryptCount)
		s.encryptCount++
		var length [2]byte
		binary.LittleEndian.PutUint16(length[:], uint16(n))
		encrypted, mac, err := chacha20poly1305.EncryptAndSeal(s.encryptKey[:], nonce[:], data[:n], length[:])
		if err != nil {
			return nil, err
		}
		buf.Write(length[:])
		buf.Write(encrypted)
		buf.Write(mac[:])
		data = data[n:]
	}
	return &buf, nil
}

func (s *controlSession) Decrypt(r io.Reader) (io.Reader, error) {
	var buf bytes.Buffer
	for {
		var length uint16
		if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if length > controlPacketLimit {
			return nil, fmt.Errorf("control packet too large: %d", length)
		}
		b := make([]byte, length)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		var mac [16]byte
		if _, err := io.ReadFull(r, mac[:]); err != nil {
			return nil, err
		}
		var nonce [8]byte
		binary.LittleEndian.PutUint64(nonce[:], s.decryptCount)
		s.decryptCount++
		var lengthBytes [2]byte
		binary.LittleEndian.PutUint16(lengthBytes[:], length)
		plain, err := chacha20poly1305.DecryptAndVerify(s.decryptKey[:], nonce[:], b, mac, lengthBytes[:])
		if err != nil {
			return nil, fmt.Errorf("control packet decryption failed: %w", err)
		}
		buf.Write(plain)
		if length < controlPacketLimit {
			break
		}
	}
	return &buf, nil
}
