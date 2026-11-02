package event

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/brutella/hc/crypto/hkdf"
	"howett.net/plist"
)

func TestSendMediaRemoteCommandWritesEncryptedRequest(t *testing.T) {
	if err := SendMediaRemoteCommand(CommandNext); err != ErrNoSession {
		t.Fatalf("without a session: %v", err)
	}
	secret := bytes.Repeat([]byte{7}, 64)
	receiverSide, senderSide := net.Pipe()
	defer receiverSide.Close()
	defer senderSide.Close()
	s := &session{conn: receiverSide, writer: &eventWriter{writer: receiverSide, key: outgoingKey(secret)}}
	setCurrent(s)
	defer clearCurrent(s)

	errs := make(chan error, 1)
	go func() { errs <- SendMediaRemoteCommand(CommandNext) }()

	// The sender decrypts with the receiver's write key.
	senderKey, _ := hkdf.Sha512(secret, []byte("Events-Salt"), []byte("Events-Write-Encryption-Key"))
	reader := bufio.NewReader(&eventReader{reader: senderSide, key: senderKey})
	line, _ := reader.ReadString('\n')
	if line != "POST /command RTSP/1.0\r\n" {
		t.Fatalf("request line %q", line)
	}
	length := 0
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if header == "\r\n" {
			break
		}
		if strings.HasPrefix(header, "Content-Length: ") {
			length, _ = strconv.Atoi(strings.TrimSpace(header[len("Content-Length: "):]))
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	var msg map[string]interface{}
	if _, err := plist.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["type"] != "sendMediaRemoteCommand" || msg["modernMediaRemoteCommand"] != uint64(CommandNext) {
		t.Fatalf("body %#v", msg)
	}
}

func TestResponsesOnEventChannelAreRead(t *testing.T) {
	input := "RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: 0\r\n\r\nRTSP/1.0 400 Bad Request\r\nContent-Length: 0\r\n\r\n"
	inspectEvents(bufio.NewReader(strings.NewReader(input))) // must consume both without panicking
}
