package handlers

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"goplay2/rtsp"
)

// Password protection. When the receiver advertises pw=true, iOS sends
// pair-pin-start, asks the user for the password and uses it as the PIN of
// the transient pair-setup (instead of 3939); a successful pair-setup marks
// the connection as allowed. Any other connection must answer an RFC 2617
// Digest challenge (as with UxPlay's -pw) before it may start or control a
// session.

const (
	digestRealm         = "raop"
	maxPasswordFailures = 5
)

type passwordAuth struct {
	password string
	allowed  map[*rtsp.Conn]bool
	nonces   map[*rtsp.Conn]string
	failures map[*rtsp.Conn]int
}

func newPasswordAuth(password string) passwordAuth {
	return passwordAuth{
		password: password,
		allowed:  make(map[*rtsp.Conn]bool),
		nonces:   make(map[*rtsp.Conn]string),
		failures: make(map[*rtsp.Conn]int),
	}
}

// needsPassword reports whether a request starts or controls playback.
// Discovery and pairing stay open, as the sender needs them to show the
// password prompt.
func needsPassword(req *rtsp.Request) bool {
	switch req.Method {
	case "GET", "OPTIONS":
		return false
	case "POST":
		switch req.Path {
		case "pair-pin-start", "pair-setup", "pair-verify", "fp-setup", "feedback":
			return false
		}
	}
	return true
}

// check returns nil when the request may proceed, or the challenge to send.
// An error closes the connection.
func (a *passwordAuth) check(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	if a.password == "" || a.allowed[conn] {
		return nil, nil
	}
	if a.allowed == nil {
		*a = newPasswordAuth(a.password)
	}
	if values, found := req.Header["Authorization"]; found && len(values) > 0 {
		nonce := a.nonces[conn]
		if nonce != "" && verifyDigest(string(req.Method), values[0], a.password, nonce) {
			a.allowed[conn] = true
			delete(a.nonces, conn)
			delete(a.failures, conn)
			log.Print("AUTH password accepted")
			return nil, nil
		}
		a.failures[conn]++
		log.Printf("AUTH wrong password (attempt %d)", a.failures[conn])
		if a.failures[conn] >= maxPasswordFailures {
			return nil, fmt.Errorf("too many wrong passwords")
		}
	} else {
		log.Print("AUTH password required")
	}
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	a.nonces[conn] = nonce
	return &rtsp.Response{StatusCode: rtsp.StatusUnauthorized, Header: rtsp.Header{
		"WWW-Authenticate": rtsp.HeaderValue{`Digest realm="` + digestRealm + `", nonce="` + nonce + `"`},
	}}, nil
}

// pin is the transient pair-setup PIN: the password when one is set.
func (a *passwordAuth) pin() string {
	if a.password != "" {
		return a.password
	}
	return transientPin
}

func (a *passwordAuth) allow(conn *rtsp.Conn) {
	if a.allowed == nil {
		*a = newPasswordAuth(a.password)
	}
	a.allowed[conn] = true
	delete(a.nonces, conn)
	delete(a.failures, conn)
}

func (a *passwordAuth) forget(conn *rtsp.Conn) {
	delete(a.allowed, conn)
	delete(a.nonces, conn)
	delete(a.failures, conn)
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func md5Hex(parts ...string) string {
	sum := md5.Sum([]byte(strings.Join(parts, ":")))
	return hex.EncodeToString(sum[:])
}

// verifyDigest checks an Authorization header against the password and the
// nonce this receiver issued.
func verifyDigest(method, header, password, nonce string) bool {
	const prefix = "digest "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	p := parseDigestParams(header[len(prefix):])
	if subtle.ConstantTimeCompare([]byte(p["nonce"]), []byte(nonce)) != 1 || p["response"] == "" {
		return false
	}
	ha1 := md5Hex(p["username"], p["realm"], password)
	ha2 := md5Hex(method, p["uri"])
	var expected string
	if qop := p["qop"]; qop != "" {
		expected = md5Hex(ha1, nonce, p["nc"], p["cnonce"], qop, ha2)
	} else {
		expected = md5Hex(ha1, nonce, ha2)
	}
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(p["response"])), []byte(expected)) == 1
}

// parseDigestParams splits `key="value", key=value` pairs; commas inside
// quotes are kept.
func parseDigestParams(s string) map[string]string {
	params := make(map[string]string)
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,\t")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = s[eq+1:]
		var value string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				value, s = s[1:], ""
			} else {
				value, s = s[1:end+1], s[end+2:]
			}
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				value, s = strings.TrimSpace(s), ""
			} else {
				value, s = strings.TrimSpace(s[:end]), s[end+1:]
			}
		}
		params[key] = value
	}
	return params
}
