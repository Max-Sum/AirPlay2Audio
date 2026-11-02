package audio

import (
	"encoding/binary"
	"errors"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/netutil"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// A control session may set up several successive audio connections, for
// example on a track change. Each connection owns its own decoder.
type Server struct {
	// Buffered streams: the timestamp mapping shared by packets and anchors,
	// and the last anchor, mapped again when the source rate changes.
	rtpMap     *rtpMapper
	anchorRTP  uint32
	anchorTime time.Time
	anchored   bool
	playing    bool

	player        *Player
	mu            sync.Mutex
	generation    uint64
	flushEpoch    atomic.Uint64
	// restart counts playback restarts (pause, immediate flush, resume
	// after standby). The buffered decoder mutes its first AAC block after
	// each, as shairport-sync does at the start of every play sequence.
	restart       atomic.Uint64
	listener      net.Listener
	connection    net.Conn
	dataSocket    *net.UDPConn
	controlSocket *net.UDPConn
	timing        io.Closer // AirPlay 1 NTP client
	done          chan struct{}
	lastActivity  atomic.Int64 // UnixNano of the last audio, control or timing packet
	gate          streamGate   // see gate.go
	// OnResume, when set before the stream is set up, is called when this
	// stream is on standby and its sender starts playing again (gate.go).
	OnResume func()
}

func NewServer(player *Player) (*Server, error) {
	s := &Server{player: player}
	s.touch()
	return s, nil
}

func (s *Server) touch() { s.lastActivity.Store(time.Now().UnixNano()) }

// LastActivity reports when data last arrived on any of the stream's sockets.
func (s *Server) LastActivity() time.Time { return time.Unix(0, s.lastActivity.Load()) }

// closeLocked closes the current connection's sockets. Called with s.mu held.
func (s *Server) closeLocked() {
	if s.listener != nil {
		s.listener.Close()
	}
	if s.connection != nil {
		s.connection.Close()
	}
	if s.dataSocket != nil {
		s.dataSocket.Close()
	}
	if s.controlSocket != nil {
		s.controlSocket.Close()
	}
	if s.timing != nil {
		s.timing.Close()
	}
	s.listener, s.connection = nil, nil
	s.dataSocket, s.controlSocket, s.timing = nil, nil, nil
	s.gate.kind, s.gate.codec, s.gate.latency = "", "", 0
	s.gate.haveAnchor = false
}

func listenPair() (*net.UDPConn, *net.UDPConn, error) {
	data, err := netutil.ListenUDP()
	if err != nil {
		return nil, nil, err
	}
	control, err := netutil.ListenUDP()
	if err != nil {
		data.Close()
		return nil, nil, err
	}
	return data, control, nil
}

// startUDP replaces the current connection with a UDP one. finish ends it,
// once, if it is still current.
func (s *Server) startUDP(data, control *net.UDPConn, timing io.Closer, samplesPerPacket uint32, closed string) (chan struct{}, func()) {
	s.mu.Lock()
	s.generation++
	generation := s.generation
	if s.done != nil {
		close(s.done)
	}
	done := make(chan struct{})
	s.done = done
	s.closeLocked()
	active := s.Active()
	if active {
		s.player.Reset()
	}
	if !s.player.synchronized {
		// UDP cannot apply TCP backpressure. Reserve up to about one second
		// of PCM for bursts, without forcing this delay when the queue is empty.
		frames := int((uint64(sampleRate) + uint64(samplesPerPacket) - 1) / uint64(samplesPerPacket))
		if frames < 2 {
			frames = 2
		}
		if frames > 256 {
			frames = 256
		}
		s.gate.realtimeCap = frames
		if active {
			s.player.ringBuffer.ResetCapacity(frames)
			s.player.SetLowLatencyCatchUp(true)
		}
		log.Printf("AUDIO realtime burst capacity=%d packets (up to approximately %d ms at negotiated packet size)", frames, uint64(frames)*uint64(samplesPerPacket)*1000/uint64(sampleRate))
	}
	s.dataSocket, s.controlSocket, s.timing = data, control, timing
	s.rtpMap = nil // UDP streams are 44.1 kHz; their timestamps are used as they are
	s.gate.classic = timing != nil
	if !active {
		s.pauseTiming(true)
	}
	s.touch()
	s.mu.Unlock()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			s.mu.Lock()
			if generation == s.generation {
				close(s.done)
				s.done = nil
				s.closeLocked()
				if s.Active() {
					s.player.Reset()
					s.player.clearStream()
				}
			}
			s.mu.Unlock()
			log.Print(closed)
		})
	}
	return done, finish
}

// Setup opens a buffered (type 103) audio connection. audioFormat is the
// format named in SETUP; each packet's SSRC selects its actual encoding.
func (s *Server) Setup(sharedKey []byte, audioFormat uint64) (int, error) {
	fallback := formatForAudioFormat(audioFormat)
	mapper := newRTPMapper(fallback.rate)
	decoder := newBufferedDecoder(fallback, mapper)
	decoder.onRateChange = s.reanchor
	decoder.restart = &s.restart
	decoder.onBytes = s.addBytes
	decoder.onFormat = func(f bufferedFormat) {
		bits := 0
		if f.alac {
			bits = int(f.bits)
		}
		s.noteFormat(f.rate, bits)
	}
	listener, err := netutil.ListenTCP()
	if err != nil {
		return -1, err
	}
	key := append([]byte(nil), sharedKey...)
	s.mu.Lock()
	s.rtpMap = mapper
	s.generation++
	generation := s.generation
	if s.done != nil {
		close(s.done)
	}
	done := make(chan struct{})
	s.done = done
	s.closeLocked()
	s.gate.realtimeCap, s.gate.classic = 0, false
	if s.Active() {
		s.player.Reset()
		if !s.player.synchronized {
			// Restore the small buffered-AAC queue after a realtime session.
			s.player.ringBuffer.ResetCapacity(s.player.baseCapacity)
			s.player.SetLowLatencyCatchUp(false)
		}
		s.player.setStream("buffered", fallback.codecName())
	}
	s.gate.kind, s.gate.codec = "buffered", fallback.codecName()
	s.gate.rate, s.gate.bits = 0, 0
	s.listener, s.connection = listener, nil
	s.touch()
	s.mu.Unlock()
	go s.control(listener, decoder, key, generation, done)
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func (s *Server) control(listener net.Listener, decoder *bufferedDecoder, key []byte, generation uint64, done <-chan struct{}) {
	defer listener.Close()
	defer decoder.Close()
	conn, err := listener.Accept()
	if err != nil {
		if !errors.Is(err, net.ErrClosed) {
			globals.ErrLog.Printf("accept audio: %v", err)
		}
		return
	}
	if !s.player.synchronized {
		// Limit decoded and kernel receive buffering so a sender which writes
		// far ahead is backpressured. Windows may adjust this socket hint.
		if tcp, ok := conn.(*net.TCPConn); ok {
			if err := tcp.SetReadBuffer(8192); err != nil {
				log.Printf("AUDIO cannot limit TCP receive buffer: %v", err)
			}
		}
	}
	s.mu.Lock()
	if generation != s.generation {
		s.mu.Unlock()
		conn.Close()
		return
	}
	s.connection = conn
	s.mu.Unlock()
	defer conn.Close()
	defer func() {
		s.mu.Lock()
		if generation == s.generation {
			close(s.done)
			s.done = nil
			s.listener, s.connection = nil, nil
			if s.Active() {
				s.player.Reset()
				s.player.clearStream()
			}
		}
		s.mu.Unlock()
		log.Print("AUDIO disconnected")
	}()
	log.Print("AUDIO connected")
	for {
		frame, err := decodeToPcm(conn, decoder, key)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				globals.ErrLog.Printf("decode buffered audio: %v", err)
			}
			return
		}
		s.touch()
		if frame != nil {
			// On standby, hold the frame and stop reading until resumed.
			for {
				if !s.waitActive(done) {
					return
				}
				if !s.player.pushIf(frame, done, s.Active) {
					return
				}
				if s.Active() {
					break
				}
				// Suspended while waiting for room: keep the frame for later.
			}
		}
	}
}

func decodeToPcm(reader io.Reader, decoder *bufferedDecoder, sharedKey []byte) (*PCMFrame, error) {
	var packetSize uint16
	if err := binary.Read(reader, binary.BigEndian, &packetSize); err != nil {
		return nil, err
	}
	if packetSize < 38 {
		return nil, errors.New("buffered audio packet is too short")
	}
	buffer := make([]byte, packetSize-2)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return nil, err
	}
	return NewFrame(decoder, sharedKey, buffer)
}

func (s *Server) SetRateAnchorTime(rtpTime uint32, networkTime time.Time) {
	s.mu.Lock()
	s.anchorRTP, s.anchorTime, s.anchored, s.playing = rtpTime, networkTime, true, true
	mapper := s.rtpMap
	s.mu.Unlock()
	if !s.Active() {
		return
	}
	s.startAt(mapper, rtpTime, networkTime)
}

func (s *Server) startAt(mapper *rtpMapper, rtpTime uint32, networkTime time.Time) {
	if mapper != nil {
		rtpTime = mapper.Map(rtpTime)
	}
	s.player.ControlChannel <- globals.ControlMessage{MType: globals.START, Param1: networkTime.UnixNano(), Param2: int64(rtpTime)}
}

// reanchor maps the last anchor again after the source rate of a buffered
// stream changed, so it stays on the same timeline as the packets.
func (s *Server) reanchor() {
	s.mu.Lock()
	rtpTime, networkTime, mapper := s.anchorRTP, s.anchorTime, s.rtpMap
	ok := s.anchored && s.playing
	s.mu.Unlock()
	if ok && s.Active() {
		s.startAt(mapper, rtpTime, networkTime)
	}
}

func (s *Server) Teardown() {
	s.mu.Lock()
	s.playing = false
	s.generation++
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	s.closeLocked()
	active := s.Active()
	if active {
		s.player.Reset()
		s.player.clearStream()
	}
	s.mu.Unlock()
	if active {
		s.player.ControlChannel <- globals.ControlMessage{MType: globals.PAUSE}
	}
}

func (s *Server) SetRate0() {
	s.restart.Add(1)
	s.mu.Lock()
	s.playing = false
	s.mu.Unlock()
	if s.Active() {
		s.player.ControlChannel <- globals.ControlMessage{MType: globals.PAUSE}
	}
}

func (s *Server) ResetBuffer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushEpoch.Add(1)
	s.restart.Add(1)
	s.gate.flushes = nil
	if s.Active() {
		s.player.Reset()
	}
}

// Flush is a deferred FLUSHBUFFERED: the packets from fromSeq up to untilSeq
// are dropped, queued or not, while playback continues.
func (s *Server) Flush(fromSeq, untilSeq uint64) {
	s.mu.Lock()
	if !s.Active() {
		// Applied when the stream resumes; the sender usually flushes just
		// before it starts playing again.
		if len(s.gate.flushes) < maxFlushRequests {
			s.gate.flushes = append(s.gate.flushes, flushRequest{from: uint32(fromSeq) & SeqMask, until: uint32(untilSeq) & SeqMask})
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	// Establish the ingress filter before acknowledging FLUSHBUFFERED. A
	// producer waiting for ring space may already hold a frame in this range.
	s.player.skipUntil(int64(fromSeq), int64(untilSeq))
}

// FlushImmediate is an immediate FLUSHBUFFERED (no flushFromSeq): queued
// audio and every packet before untilSeq are dropped, and playback stops
// until the sender anchors it again with SETRATEANCHORTIME.
func (s *Server) FlushImmediate(untilSeq uint64) {
	s.restart.Add(1)
	s.mu.Lock()
	s.playing = false
	if !s.Active() {
		s.gate.flushes = append(s.gate.flushes[:0], flushRequest{immediate: true, until: uint32(untilSeq) & SeqMask})
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.player.flushImmediate(untilSeq)
	s.player.ControlChannel <- globals.ControlMessage{MType: globals.PAUSE}
}
