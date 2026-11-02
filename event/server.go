package event

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/brutella/hc/crypto/chacha20poly1305"
	"github.com/brutella/hc/crypto/hkdf"
	"goplay2/globals"
	"howett.net/plist"
)

var pairingKeys sync.Map // remote IP -> short-lived pairing secret ([]byte)

// RegisterPairing makes the verified session secret available to the event
// connection which the same sender opens during SETUP. The secret is the
// pair-verify shared key, or the SRP session key for transient pairing.
func RegisterPairing(address net.Addr, sharedKey []byte) {
	host, _, err := net.SplitHostPort(address.String())
	if err == nil {
		pairingKeys.Store(host, append([]byte(nil), sharedKey...))
	}
}

func Serve(listener net.Listener) {
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleEventConnection(conn)
	}
}

// The sender opens the event channel; the receiver uses it to send requests
// such as remote control commands back to the sender. Anything arriving on
// it (responses, or requests from the sender) is decrypted and logged.
func handleEventConnection(conn net.Conn) {
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return
	}
	value, ok := pairingKeys.LoadAndDelete(host)
	if !ok {
		log.Print("EVENT connection has no verified session")
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	secret := value.([]byte)
	s := &session{conn: conn, writer: &eventWriter{writer: conn, key: outgoingKey(secret)}, host: host}
	setCurrent(s)
	defer clearCurrent(s)
	log.Print("EVENT channel connected")
	go func() {
		if err := s.sendUpdateInfo(); err != nil {
			log.Printf("EVENT updateInfo not sent: %v", err)
		}
	}()
	inspectEvents(bufio.NewReader(&eventReader{reader: conn, key: incomingKey(secret)}))
	log.Print("EVENT channel closed")
}

func incomingKey(secret []byte) [32]byte {
	key, _ := hkdf.Sha512(secret, []byte("Events-Salt"), []byte("Events-Read-Encryption-Key"))
	return key
}

// outgoingKey encrypts what the receiver writes. Like the control channel,
// each direction has its own key.
func outgoingKey(secret []byte) [32]byte {
	key, _ := hkdf.Sha512(secret, []byte("Events-Salt"), []byte("Events-Write-Encryption-Key"))
	return key
}

func inspectEvents(reader *bufio.Reader) {
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			return
		}
		first := strings.Fields(string(line))
		response := len(first) >= 2 && strings.HasPrefix(first[0], "RTSP/")
		if !response && len(first) != 3 {
			return
		}
		length, headerBytes, contentType := 0, len(line), ""
		for {
			line, err = reader.ReadSlice('\n')
			if err != nil {
				return
			}
			headerBytes += len(line)
			if headerBytes > 16384 {
				return
			}
			if string(line) == "\r\n" {
				break
			}
			parts := strings.SplitN(string(line), ":", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Content-Type") {
				contentType = strings.TrimSpace(parts[1])
			}
			if len(parts) == 2 && strings.EqualFold(parts[0], "Content-Length") {
				length, err = strconv.Atoi(strings.TrimSpace(parts[1]))
				if err != nil || length < 0 || length > 128*1024 {
					return
				}
			}
		}
		body := make([]byte, length)
		if _, err = io.ReadFull(reader, body); err != nil {
			return
		}
		if globals.Trace {
			log.Printf("DIAG event %s", describeEvent(first, contentType, body))
		}
		if response {
			status := first[1]
			if _, err := strconv.Atoi(status); err != nil {
				status = "?"
			}
			log.Printf("EVENT response status=%s body=%d", status, len(body))
			continue
		}
		if first[0] == "POST" && first[1] == "/command" {
			var msg struct {
				Type string `plist:"type"`
			}
			if _, err := plist.Unmarshal(body, &msg); err == nil && isIdentifier(msg.Type) {
				log.Printf("EVENT command %s", msg.Type)
				continue
			}
		}
		log.Print("EVENT message received")
	}
}

// describeEvent summarizes an event message: request line, content type,
// size and plist field names. Field values are not included.
func describeEvent(first []string, contentType string, body []byte) string {
	clean := func(value string) string {
		value = strings.Map(func(c rune) rune {
			if c <= 0x20 || c == 0x7f {
				return -1
			}
			return c
		}, value)
		if len(value) > 64 {
			value = value[:64]
		}
		return value
	}
	parts := make([]string, 0, 3)
	for i := 0; i < len(first) && i < 3; i++ {
		parts = append(parts, clean(first[i]))
	}
	text := fmt.Sprintf("%s type=%s body=%d", strings.Join(parts, " "), clean(contentType), len(body))
	var content map[string]interface{}
	if _, err := plist.Unmarshal(body, &content); err == nil {
		keys := make([]string, 0, len(content))
		for key := range content {
			if isIdentifier(key) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		text += " fields=[" + strings.Join(keys, " ") + "]"
		if params, ok := content["params"].(map[string]interface{}); ok {
			keys = keys[:0]
			for key := range params {
				if isIdentifier(key) {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			text += " params=[" + strings.Join(keys, " ") + "]"
		}
	}
	return text
}

func isIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

type eventReader struct {
	reader  io.Reader
	key     [32]byte
	counter uint64
	pending []byte
}

func (r *eventReader) Read(out []byte) (int, error) {
	if len(r.pending) == 0 {
		var length [2]byte
		if _, err := io.ReadFull(r.reader, length[:]); err != nil {
			return 0, err
		}
		n := binary.LittleEndian.Uint16(length[:])
		if n > 1024 {
			return 0, fmt.Errorf("oversized encrypted event frame")
		}
		ciphertext := make([]byte, int(n))
		if _, err := io.ReadFull(r.reader, ciphertext); err != nil {
			return 0, err
		}
		var tag [16]byte
		if _, err := io.ReadFull(r.reader, tag[:]); err != nil {
			return 0, err
		}
		var nonce [8]byte
		binary.LittleEndian.PutUint64(nonce[:], r.counter)
		r.counter++
		plain, err := chacha20poly1305.DecryptAndVerify(r.key[:], nonce[:], ciphertext, tag, length[:])
		if err != nil {
			return 0, err
		}
		r.pending = plain
	}
	n := copy(out, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
