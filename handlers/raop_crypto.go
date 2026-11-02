package handlers

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"sync"
)

// AirPlay 1 senders encrypt the audio key with the public key of the
// original AirPort Express and check that the receiver can sign their
// Apple-Challenge with it. The private key has been public since 2011 and is
// used by every open receiver, including shairport-sync.
const airportKeyPEM = `-----BEGIN RSA PRIVATE KEY-----
MIIEpQIBAAKCAQEA59dE8qLieItsH1WgjrcFRKj6eUWqi+bGLOX1HL3U3GhC/j0Qg90u3sG/1CUt
wC5vOYvfDmFI6oSFXi5ELabWJmT2dKHzBJKa3k9ok+8t9ucRqMd6DZHJ2YCCLlDRKSKv6kDqnw4U
wPdpOMXziC/AMj3Z/lUVX1G7WSHCAWKf1zNS1eLvqr+boEjXuBOitnZ/bDzPHrTOZz0Dew0uowxf
/+sG+NCK3eQJVxqcaJ/vEHKIVd2M+5qL71yJQ+87X6oV3eaYvt3zWZYD6z5vYTcrtij2VZ9Zmni/
UAaHqn9JdsBWLUEpVviYnhimNVvYFZeCXg/IdTQ+x4IRdiXNv5hEewIDAQABAoIBAQDl8Axy9XfW
BLmkzkEiqoSwF0PsmVrPzH9KsnwLGH+QZlvjWd8SWYGN7u1507HvhF5N3drJoVU3O14nDY4TFQAa
LlJ9VM35AApXaLyY1ERrN7u9ALKd2LUwYhM7Km539O4yUFYikE2nIPscEsA5ltpxOgUGCY7b7ez5
NtD6nL1ZKauw7aNXmVAvmJTcuPxWmoktF3gDJKK2wxZuNGcJE0uFQEG4Z3BrWP7yoNuSK3dii2jm
lpPHr0O/KnPQtzI3eguhe0TwUem/eYSdyzMyVx/YpwkzwtYL3sR5k0o9rKQLtvLzfAqdBxBurciz
aaA/L0HIgAmOit1GJA2saMxTVPNhAoGBAPfgv1oeZxgxmotiCcMXFEQEWflzhWYTsXrhUIuz5jFu
a39GLS99ZEErhLdrwj8rDDViRVJ5skOp9zFvlYAHs0xh92ji1E7V/ysnKBfsMrPkk5KSKPrnjndM
oPdevWnVkgJ5jxFuNgxkOLMuG9i53B4yMvDTCRiIPMQ++N2iLDaRAoGBAO9v//mU8eVkQaoANf0Z
oMjW8CN4xwWA2cSEIHkd9AfFkftuv8oyLDCG3ZAf0vrhrrtkrfa7ef+AUb69DNggq4mHQAYBp7L+
k5DKzJrKuO0r+R0YbY9pZD1+/g9dVt91d6LQNepUE/yY2PP5CNoFmjedpLHMOPFdVgqDzDFxU8hL
AoGBANDrr7xAJbqBjHVwIzQ4To9pb4BNeqDndk5Qe7fT3+/H1njGaC0/rXE0Qb7q5ySgnsCb3DvA
cJyRM9SJ7OKlGt0FMSdJD5KG0XPIpAVNwgpXXH5MDJg09KHeh0kXo+QA6viFBi21y340NonnEfdf
54PX4ZGS/Xac1UK+pLkBB+zRAoGAf0AY3H3qKS2lMEI4bzEFoHeK3G895pDaK3TFBVmD7fV0Zhov
17fegFPMwOII8MisYm9ZfT2Z0s5Ro3s5rkt+nvLAdfC/PYPKzTLalpGSwomSNYJcB9HNMlmhkGzc
1JnLYT4iyUyx6pcZBmCd8bD0iwY/FzcgNDaUmbX9+XDvRA0CgYEAkE7pIPlE71qvfJQgoA9em0gI
LAuE4Pu13aKiJnfft7hIjbK+5kyb3TysZvoyDnb3HOKvInK7vXbKuU4ISgxB2bB3HcYzQMGsz1qJ
2gG0N5hvJpzwwhbhXqFKA4zaaSrw622wDniAK5MlIE0tIAKKP4yxNGjoD2QYjhBGuhvkWKY=
-----END RSA PRIVATE KEY-----`

var (
	airportKeyOnce sync.Once
	airportKey     *rsa.PrivateKey
	airportKeyErr  error
)

func loadAirportKey() (*rsa.PrivateKey, error) {
	airportKeyOnce.Do(func() {
		block, _ := pem.Decode([]byte(airportKeyPEM))
		if block == nil {
			airportKeyErr = fmt.Errorf("invalid AirPlay 1 key")
			return
		}
		airportKey, airportKeyErr = x509.ParsePKCS1PrivateKey(block.Bytes)
	})
	return airportKey, airportKeyErr
}

// AirPlay1PublicKey is the key senders encrypt to, for test tools.
func AirPlay1PublicKey() (*rsa.PublicKey, error) {
	key, err := loadAirportKey()
	if err != nil {
		return nil, err
	}
	return &key.PublicKey, nil
}

// decodeBase64 accepts the unpadded base64 AirPlay 1 senders use.
func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "=")
	return base64.RawStdEncoding.DecodeString(s)
}

// appleResponse signs the Apple-Challenge: the decoded challenge, the
// receiver's address on this connection and its hardware address, padded
// with zeros to 32 bytes, signed with raw PKCS#1 v1.5 padding.
func appleResponse(challenge string, local net.IP, hardware net.HardwareAddr) (string, error) {
	key, err := loadAirportKey()
	if err != nil {
		return "", err
	}
	data, err := decodeBase64(challenge)
	if err != nil {
		return "", fmt.Errorf("Apple-Challenge: %w", err)
	}
	if ip4 := local.To4(); ip4 != nil {
		data = append(data, ip4...)
	} else {
		data = append(data, local.To16()...)
	}
	data = append(data, hardware...)
	for len(data) < 32 {
		data = append(data, 0)
	}
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.Hash(0), data)
	if err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(signature), nil
}

// decryptAESKey recovers the session's AES key from the SDP rsaaeskey.
func decryptAESKey(encoded string) ([]byte, error) {
	key, err := loadAirportKey()
	if err != nil {
		return nil, err
	}
	data, err := decodeBase64(encoded)
	if err != nil {
		return nil, err
	}
	aesKey, err := rsa.DecryptOAEP(sha1.New(), nil, key, data, nil)
	if err != nil {
		return nil, err
	}
	if len(aesKey) != 16 {
		return nil, fmt.Errorf("AES key has %d bytes", len(aesKey))
	}
	return aesKey, nil
}
