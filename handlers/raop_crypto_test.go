package handlers

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"strings"
	"testing"
)

func TestAirportKeyIsValid(t *testing.T) {
	key, err := loadAirportKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Validate(); err != nil {
		t.Fatalf("key does not validate: %v", err)
	}
	if key.N.BitLen() != 2048 {
		t.Fatalf("modulus has %d bits", key.N.BitLen())
	}
	// The well-known public modulus begins with e7d744f2.
	if !strings.HasPrefix(key.N.Text(16), "e7d744f2") {
		t.Fatalf("unexpected modulus %s…", key.N.Text(16)[:16])
	}
}

func TestAppleResponseVerifiesWithPublicKey(t *testing.T) {
	key, _ := loadAirportKey()
	challenge := make([]byte, 16)
	rand.Read(challenge)
	ip := net.IPv4(192, 168, 1, 20)
	mac := net.HardwareAddr{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	response, err := appleResponse(strings.TrimRight(base64.StdEncoding.EncodeToString(challenge), "="), ip, mac)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(response, "=") {
		t.Fatal("response must not be padded")
	}
	signature, _ := base64.RawStdEncoding.DecodeString(response)
	expected := append(append(append([]byte{}, challenge...), ip.To4()...), mac...)
	expected = append(expected, make([]byte, 32-len(expected))...)
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.Hash(0), expected, signature); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	// IPv6 connections sign the 16-byte address.
	if _, err := appleResponse(base64.StdEncoding.EncodeToString(challenge), net.ParseIP("fe80::1"), mac); err != nil {
		t.Fatal(err)
	}
}

func TestDecryptAESKey(t *testing.T) {
	key, _ := loadAirportKey()
	aes := []byte("0123456789abcdef")
	encrypted, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &key.PublicKey, aes, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decryptAESKey(base64.RawStdEncoding.EncodeToString(encrypted))
	if err != nil || string(got) != string(aes) {
		t.Fatalf("got %q, %v", got, err)
	}
}
