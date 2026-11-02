package event

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/brutella/hc/crypto/chacha20poly1305"
	"howett.net/plist"
)

// MediaRemote command numbers, as listed by the sender in
// updateMRSupportedCommands.
const (
	CommandPlay     = 0
	CommandPause    = 1
	CommandToggle   = 2
	CommandStop     = 3
	CommandNext     = 4
	CommandPrevious = 5
)

// ErrNoSession is returned when no sender has an event channel open.
var ErrNoSession = errors.New("no sender connected on the event channel")

type session struct {
	mu     sync.Mutex
	conn   net.Conn
	writer *eventWriter
	cseq   int
	host   string // sender address, as in the RTSP connection
}

// Several senders may be connected at once, each with its own event
// channel. Commands from the window go to the sender that is playing.
var (
	currentMu  sync.Mutex
	current    *session            // most recently connected
	byHost     = map[string]*session{}
	activeHost string
)

func setCurrent(s *session) {
	currentMu.Lock()
	current = s
	if s.host != "" {
		byHost[s.host] = s
	}
	currentMu.Unlock()
}

func clearCurrent(s *session) {
	currentMu.Lock()
	if current == s {
		current = nil
	}
	if byHost[s.host] == s {
		delete(byHost, s.host)
	}
	currentMu.Unlock()
}

// SetActiveSender names the sender (its IP address) whose event channel
// receives SendMediaRemoteCommand; empty means the latest channel.
func SetActiveSender(host string) {
	currentMu.Lock()
	activeHost = host
	currentMu.Unlock()
}

// Remote is another way to control a sender: DACP, which AirPlay 1 senders
// offer instead of an event channel.
type Remote interface {
	Command(command uint64) error
	ID() string
}

var remotes = map[string]Remote{} // sender address -> remote control; guarded by currentMu

// RegisterRemote makes a sender's DACP remote control available. It is used
// when the sender has no event channel (AirPlay 1).
func RegisterRemote(host string, remote Remote) {
	currentMu.Lock()
	remotes[host] = remote
	currentMu.Unlock()
}

// UnregisterRemote forgets the remote control with this DACP-ID.
func UnregisterRemote(host, id string) {
	currentMu.Lock()
	if r := remotes[host]; r != nil && r.ID() == id {
		delete(remotes, host)
	}
	currentMu.Unlock()
}

// SendMediaRemoteCommand asks the playing sender to perform a playback
// command, such as CommandNext. The sender's response is logged by the reader.
func SendMediaRemoteCommand(command uint64) error {
	currentMu.Lock()
	s, host := current, activeHost
	if host != "" {
		s = byHost[host]
	}
	remote := remotes[host]
	currentMu.Unlock()
	if s == nil && remote != nil {
		return remote.Command(command)
	}
	return s.sendCommand(command)
}

// SendMediaRemoteCommandTo sends a command to the sender at host, over its
// event channel or else its DACP remote control.
func SendMediaRemoteCommandTo(host string, command uint64) error {
	currentMu.Lock()
	s, remote := byHost[host], remotes[host]
	currentMu.Unlock()
	if s == nil && remote != nil {
		return remote.Command(command)
	}
	return s.sendCommand(command)
}

// InfoProvider returns the receiver's /info dictionary, sent to each sender
// as updateInfo when its event channel connects.
var InfoProvider func() interface{}

// sendUpdateInfo sends updateInfo, as shairport-sync does once the event
// channel is connected, so the sender learns the receiver's current state
// (group, status flags, volume) without asking for /info again.
func (s *session) sendUpdateInfo() error {
	if InfoProvider == nil {
		return nil
	}
	body, err := plist.Marshal(map[string]interface{}{
		"type":  "updateInfo",
		"value": InfoProvider(),
	}, plist.BinaryFormat)
	if err != nil {
		return err
	}
	return s.post("/command", body)
}

func (s *session) sendCommand(command uint64) error {
	if s == nil {
		return ErrNoSession
	}
	body, err := plist.Marshal(map[string]interface{}{
		"type":                     "sendMediaRemoteCommand",
		"modernMediaRemoteCommand": command,
	}, plist.BinaryFormat)
	if err != nil {
		return err
	}
	return s.post("/command", body)
}

func (s *session) post(path string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cseq++
	request := fmt.Sprintf("POST %s RTSP/1.0\r\nCSeq: %d\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: %d\r\n\r\n", path, s.cseq, len(body))
	_ = s.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	defer s.conn.SetWriteDeadline(time.Time{})
	return s.writer.Write(append([]byte(request), body...))
}

// eventWriter encrypts data in the same framing as eventReader: a 2-byte
// little-endian length, the ciphertext and a 16-byte tag, with a per
// direction counter as nonce.
type eventWriter struct {
	writer  io.Writer
	key     [32]byte
	counter uint64
}

func (w *eventWriter) Write(data []byte) error {
	for len(data) > 0 {
		n := len(data)
		if n > 1024 {
			n = 1024
		}
		var length [2]byte
		binary.LittleEndian.PutUint16(length[:], uint16(n))
		var nonce [8]byte
		binary.LittleEndian.PutUint64(nonce[:], w.counter)
		w.counter++
		ciphertext, tag, err := chacha20poly1305.EncryptAndSeal(w.key[:], nonce[:], data[:n], length[:])
		if err != nil {
			return err
		}
		frame := append(append(length[:], ciphertext...), tag[:]...)
		if _, err := w.writer.Write(frame); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// LogCommand sends a command and logs the outcome, for the console and window.
func LogCommand(name string, command uint64) {
	if err := SendMediaRemoteCommand(command); err != nil {
		log.Printf("MEDIA command %s failed: %v", name, err)
		return
	}
	log.Printf("MEDIA command %s sent", name)
}
