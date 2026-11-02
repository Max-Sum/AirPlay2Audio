package handlers

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/brutella/hc/hap/pair"
	"github.com/brutella/hc/util"
	"github.com/Max-Sum/AirPlay2Audio/homekit"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
)

func TestVerifyDigestRFC2617Example(t *testing.T) {
	header := `Digest username="Mufasa", realm="testrealm@host.com", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", ` +
		`uri="/dir/index.html", qop=auth, nc=00000001, cnonce="0a4f113b", ` +
		`response="6629fae49393a05397450978507c4ef1", opaque="5ccc069c403ebaf9f0171e9517f40e41"`
	if !verifyDigest("GET", header, "Circle Of Life", "dcd98b7102dd2f0e8b11d0f600bfb0c093") {
		t.Fatal("valid digest rejected")
	}
	if verifyDigest("GET", header, "wrong", "dcd98b7102dd2f0e8b11d0f600bfb0c093") {
		t.Fatal("wrong password accepted")
	}
	if verifyDigest("GET", header, "Circle Of Life", "another-nonce") {
		t.Fatal("foreign nonce accepted")
	}
}

func digestFor(method, uri, password, challenge string) string {
	nonce := strings.TrimSuffix(strings.SplitN(challenge, `nonce="`, 2)[1], `"`)
	ha1 := md5Hex("AirPlay", digestRealm, password)
	ha2 := md5Hex(method, uri)
	return `Digest username="AirPlay", realm="raop", nonce="` + nonce + `", uri="` + uri +
		`", response="` + md5Hex(ha1, nonce, ha2) + `"`
}

func TestPasswordChallenge(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := &rtsp.Conn{}
	conn.SetNetConn(left)
	auth := newPasswordAuth("1234")
	setup := &rtsp.Request{Method: "SETUP", Path: "rtsp://receiver/1", Header: rtsp.Header{}}

	resp, err := auth.check(conn, setup)
	if err != nil || resp == nil || resp.StatusCode != rtsp.StatusUnauthorized {
		t.Fatalf("expected challenge, got %v %v", resp, err)
	}
	challenge := resp.Header["WWW-Authenticate"][0]

	setup.Header["Authorization"] = rtsp.HeaderValue{digestFor("SETUP", "rtsp://receiver/1", "0000", challenge)}
	if resp, _ = auth.check(conn, setup); resp == nil || resp.StatusCode != rtsp.StatusUnauthorized {
		t.Fatal("wrong password accepted")
	}
	challenge = resp.Header["WWW-Authenticate"][0]

	setup.Header["Authorization"] = rtsp.HeaderValue{digestFor("SETUP", "rtsp://receiver/1", "1234", challenge)}
	if resp, err = auth.check(conn, setup); resp != nil || err != nil {
		t.Fatalf("correct password rejected: %v %v", resp, err)
	}
	// Later requests on the same connection need no new answer.
	if resp, _ = auth.check(conn, &rtsp.Request{Method: "RECORD", Header: rtsp.Header{}}); resp != nil {
		t.Fatal("authenticated connection challenged again")
	}
}

func TestPasswordTooManyFailures(t *testing.T) {
	conn := &rtsp.Conn{}
	auth := newPasswordAuth("1234")
	req := &rtsp.Request{Method: "SETUP", Header: rtsp.Header{}}
	resp, _ := auth.check(conn, req)
	var err error
	for i := 0; i < maxPasswordFailures && err == nil; i++ {
		req.Header["Authorization"] = rtsp.HeaderValue{digestFor("SETUP", "x", "bad", resp.Header["WWW-Authenticate"][0])}
		resp, err = auth.check(conn, req)
	}
	if err == nil {
		t.Fatal("connection not closed after repeated wrong passwords")
	}
}

// transientPair runs pair-setup M1-M4 with the given PIN and reports whether
// the receiver accepted it.
func transientPair(t *testing.T, handler *Rstp, connection *rtsp.Conn, pin string) bool {
	t.Helper()
	m1 := util.NewTLV8Container()
	m1.SetByte(pair.TagSequence, 1)
	m1.SetByte(tagFlags, pairFlagTransient)
	resp, err := handler.OnPairSetup(connection, &rtsp.Request{Body: m1.BytesBuffer().Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := util.NewTLV8ContainerFromReader(bytes.NewReader(resp.Body))
	client := pair.NewSetupClientSession("Pair-Setup", pin)
	if err := client.GenerateKeys(m2.GetBytes(pair.TagSalt), m2.GetBytes(pair.TagPublicKey)); err != nil {
		t.Fatal(err)
	}
	m3 := util.NewTLV8Container()
	m3.SetByte(pair.TagSequence, 3)
	m3.SetBytes(pair.TagPublicKey, client.PublicKey)
	m3.SetBytes(pair.TagProof, client.Proof)
	resp, err = handler.OnPairSetup(connection, &rtsp.Request{Body: m3.BytesBuffer().Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	m4, _ := util.NewTLV8ContainerFromReader(bytes.NewReader(resp.Body))
	return m4.GetByte(pair.TagErrCode) == 0
}

func TestPasswordIsTransientPin(t *testing.T) {
	server, err := homekit.NewServer("02:00:00:00:00:04", "Test", t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	homekit.Server = server
	newConn := func() *rtsp.Conn {
		left, right := net.Pipe()
		t.Cleanup(func() { left.Close(); right.Close() })
		c := &rtsp.Conn{}
		c.SetNetConn(newSecureConn(left, server.Context))
		return c
	}
	handler := &Rstp{transient: make(map[string]*pair.SetupServerSession), auth: newPasswordAuth("secret")}

	wrong := newConn()
	if transientPair(t, handler, wrong, transientPin) {
		t.Fatal("default PIN accepted although a password is set")
	}
	if resp, _ := handler.auth.check(wrong, &rtsp.Request{Method: "SETUP", Header: rtsp.Header{}}); resp == nil {
		t.Fatal("connection with wrong password may start a session")
	}

	right := newConn()
	if !transientPair(t, handler, right, "secret") {
		t.Fatal("password rejected as PIN")
	}
	if resp, err := handler.auth.check(right, &rtsp.Request{Method: "SETUP", Header: rtsp.Header{}}); resp != nil || err != nil {
		t.Fatal("paired connection challenged again")
	}
}

func TestPairingNeedsNoPassword(t *testing.T) {
	for _, path := range []string{"pair-pin-start", "pair-setup", "pair-verify", "fp-setup"} {
		if needsPassword(&rtsp.Request{Method: "POST", Path: path}) {
			t.Fatalf("%s must stay open", path)
		}
	}
	if needsPassword(&rtsp.Request{Method: "GET", Path: "info"}) {
		t.Fatal("GET /info must stay open")
	}
	for _, method := range []rtsp.Method{"SETUP", "RECORD", "SETRATEANCHORTIME", "SET_PARAMETER", "TEARDOWN"} {
		if !needsPassword(&rtsp.Request{Method: method}) {
			t.Fatalf("%s must need the password", method)
		}
	}
}
