package handlers

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/brutella/hc/crypto"
	"github.com/brutella/hc/hap"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
)

// readTwoRequests sends a request with a large body, as a sender does for
// cover artwork, followed by a small one, and reads both through conn.
func readTwoRequests(t *testing.T, wrap func(net.Conn, hap.Context) net.Conn) error {
	t.Helper()
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{3}, 32))
	serverCrypto, _ := crypto.NewSecureSessionFromSharedKey(key)
	clientCrypto, _ := crypto.NewSecureClientSessionFromSharedKey(key)

	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	context := hap.NewContextForSecuredDevice(nil)
	conn := wrap(left, context)
	context.GetSessionForConnection(left).SetCryptographer(serverCrypto)

	body := bytes.Repeat([]byte{0xab}, 180224)
	first := append([]byte(fmt.Sprintf("SET_PARAMETER rtsp://192.168.0.11/1 RTSP/1.0\r\nCSeq: 1\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(body))), body...)
	second := []byte("GET_PARAMETER rtsp://192.168.0.11/1 RTSP/1.0\r\nCSeq: 2\r\n\r\n")
	go func() {
		for _, message := range [][]byte{first, second} {
			encrypted, err := clientCrypto.Encrypt(bytes.NewReader(message))
			if err != nil {
				return
			}
			if _, err := io.Copy(right, encrypted); err != nil {
				return
			}
		}
	}()

	reader := bufio.NewReaderSize(conn, 4096) // as the RTSP server reads
	var req rtsp.Request
	if err := req.Read(reader); err != nil {
		return fmt.Errorf("first request: %w", err)
	}
	if !bytes.Equal(req.Body, body) {
		return fmt.Errorf("first request body differs")
	}
	if err := req.Read(reader); err != nil {
		return fmt.Errorf("second request: %w", err)
	}
	if req.Method != "GET_PARAMETER" {
		return fmt.Errorf("second request method %q", req.Method)
	}
	return nil
}

func TestSecureConnReadsRequestAfterLargeBody(t *testing.T) {
	err := readTwoRequests(t, func(c net.Conn, ctx hap.Context) net.Conn { return newSecureConn(c, ctx) })
	if err != nil {
		t.Fatal(err)
	}
}

// Documents the hap.Connection fault that secureConn works around: the
// connection ends after the large body. If this starts passing, the
// dependency was fixed and secureConn may be unnecessary.
func TestHapConnectionFailsAfterLargeBody(t *testing.T) {
	err := readTwoRequests(t, func(c net.Conn, ctx hap.Context) net.Conn { return hap.NewConnection(c, ctx) })
	if err == nil {
		t.Skip("hap.Connection no longer loses the connection; secureConn may be removable")
	}
	t.Logf("hap.Connection: %v", err)
}
