//go:build windows

package handlers

// End-to-end tests of sender switching: simulated senders go through the
// real RTSP handler, real audio servers (TCP AAC and UDP ALAC), real
// decoding and the shared Player. Only the sound card is replaced, by an
// output that pulls audio in real time and records whether it was silent.

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/big"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	goptp "github.com/albanseurat/go-ptp"
	"github.com/brutella/hc/crypto/chacha20poly1305"
	"github.com/Max-Sum/AirPlay2Audio/audio"
	"github.com/Max-Sum/AirPlay2Audio/codec"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/homekit"
	"github.com/Max-Sum/AirPlay2Audio/ptp"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
	"howett.net/plist"
)

// ---- fake sound card ----

type outBlock struct {
	at    time.Time
	sound bool
}

type fakeOutput struct {
	mu     sync.Mutex
	cb     codec.StreamCallback
	stop   chan struct{}
	done   chan struct{}
	blocks []outBlock
}

func (o *fakeOutput) Init(cb codec.StreamCallback) error {
	o.mu.Lock()
	o.cb = cb
	o.mu.Unlock()
	return nil
}

func (o *fakeOutput) Start() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stop != nil {
		return nil
	}
	stop, done, cb := make(chan struct{}), make(chan struct{}), o.cb
	o.stop, o.done = stop, done
	go func() {
		defer close(done)
		out := make([]int16, 1024)
		period := 512 * time.Second / 44100
		next := time.Now()
		for {
			select {
			case <-stop:
				return
			default:
			}
			cb(out, 0, 30*time.Millisecond)
			sound := false
			for _, v := range out {
				if v > 100 || v < -100 {
					sound = true
					break
				}
			}
			o.mu.Lock()
			o.blocks = append(o.blocks, outBlock{time.Now(), sound})
			o.mu.Unlock()
			next = next.Add(period)
			if d := time.Until(next); d > 0 {
				time.Sleep(d)
			} else {
				next = time.Now()
			}
		}
	}()
	return nil
}

func (o *fakeOutput) Stop() error {
	o.mu.Lock()
	stop, done := o.stop, o.done
	o.stop, o.done = nil, nil
	o.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	return nil
}

func (o *fakeOutput) Close() error            { return o.Stop() }
func (o *fakeOutput) SetVolume(float64) error { return nil }

// soundTimes returns when sound was output after since.
func (o *fakeOutput) soundTimes(since time.Time) []time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	var times []time.Time
	for _, b := range o.blocks {
		if b.sound && b.at.After(since) {
			times = append(times, b.at)
		}
	}
	return times
}

// expectSound waits for audio after since, then requires it to continue for
// sustain without a gap longer than maxGap.
func expectSound(t *testing.T, o *fakeOutput, since time.Time, what string) {
	t.Helper()
	const sustain, maxGap = 1200 * time.Millisecond, 150 * time.Millisecond
	deadline := time.Now().Add(4 * time.Second)
	var first time.Time
	for time.Now().Before(deadline) {
		if times := o.soundTimes(since); len(times) > 0 {
			first = times[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if first.IsZero() {
		t.Fatalf("%s: no sound within 4 s", what)
	}
	time.Sleep(time.Until(first.Add(sustain)))
	previous := first
	for _, at := range o.soundTimes(first) {
		if at.After(first.Add(sustain)) {
			break
		}
		if gap := at.Sub(previous); gap > maxGap {
			t.Fatalf("%s: %v of silence %v after sound started", what, gap.Round(time.Millisecond), previous.Sub(first).Round(time.Millisecond))
		}
		previous = at
	}
	if gap := first.Add(sustain).Sub(previous); gap > maxGap {
		t.Fatalf("%s: sound stopped %v after it started", what, previous.Sub(first).Round(time.Millisecond))
	}
	t.Logf("%s: sound %v after the request", what, first.Sub(since).Round(time.Millisecond))
}

// ---- simulated senders ----

func plistBody(t *testing.T, value interface{}) []byte {
	t.Helper()
	body, err := plist.Marshal(value, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// ptpTime splits a VirtualClock time into SETRATEANCHORTIME's fields.
func ptpTime(ns int64) (uint64, uint64) {
	secs := ns / int64(time.Second)
	frac := new(big.Int).Lsh(big.NewInt(ns%int64(time.Second)), 64)
	frac.Div(frac, big.NewInt(int64(time.Second)))
	return uint64(secs), frac.Uint64()
}

type sender struct {
	t       *testing.T
	r       *Rstp
	clock   *ptp.VirtualClock
	conn    *rtsp.Conn
	peer    net.Conn
	path    string
	name    string
	key     []byte
	tsBase  uint32
	clockID uint64
	samples int // per packet
	// The sender's own PTP time is the receiver's local time plus ahead.
	ahead time.Duration
	// ptpTo, when set, makes the sender a PTP master sending to these
	// receiver ports (event, general).
	ptpTo []*net.UDPAddr
	ip    net.IP

	mu        sync.Mutex
	wake      chan struct{} // closed when playing starts
	playing   bool
	next      int   // next packet index to send
	playIndex int   // packet index anchored at playBase
	playBase  int64 // VirtualClock ns at which playIndex plays
	closed    bool

	// buffered (TCP AAC)
	audio net.Conn
	aac   [][]byte
	// realtime (UDP ALAC)
	realtime bool
	data     *net.UDPConn
	control  *net.UDPConn
}

func (s *sender) do(method, contentType string, body []byte) *rtsp.Response {
	s.t.Helper()
	req := &rtsp.Request{Method: rtsp.Method(method), Path: s.path, Header: rtsp.Header{}, Body: body}
	if contentType != "" {
		req.Header["Content-Type"] = rtsp.HeaderValue{contentType}
	}
	resp, err := s.r.Handle(s.conn, req)
	if err != nil || resp == nil || resp.StatusCode != rtsp.StatusOK {
		code := 0
		if resp != nil {
			code = int(resp.StatusCode)
		}
		s.t.Errorf("%s %s: status %d, error %v", s.name, method, code, err)
	}
	return resp
}

func (s *sender) doPlist(method string, value interface{}) *rtsp.Response {
	return s.do(method, "application/x-apple-binary-plist", plistBody(s.t, value))
}

// now is the sender's clock in nanoseconds.
func (s *sender) now() int64 { return s.clock.LocalNow().UnixNano() + int64(s.ahead) }

// runPTP acts as this sender's PTP master: Sync, Follow_Up and Delay_Resp
// every 125 ms, timestamped on the sender's clock.
func (s *sender) runPTP() {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: s.ip})
	if err != nil {
		s.t.Errorf("PTP master %s: %v", s.name, err)
		return
	}
	defer conn.Close()
	for seq := uint16(1); ; seq++ {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		header := goptp.Header{VersionPTP: goptp.Version2, ClockIdentity: s.clockID, PortNumber: 1, SequenceID: seq}
		origin := time.Unix(0, s.now())
		header.MessageType = goptp.SyncMsgType
		packet, _ := (&goptp.SyncMsg{Header: header, OriginTimestamp: origin}).MarshalBinary()
		_, _ = conn.WriteToUDP(packet, s.ptpTo[0])
		header.MessageType = goptp.FollowUpMsgType
		packet, _ = (&goptp.FollowUpMsg{Header: header, PreciseOriginTimestamp: origin}).MarshalBinary()
		_, _ = conn.WriteToUDP(packet, s.ptpTo[1])
		time.Sleep(5 * time.Millisecond) // the receiver sends its Delay_Req meanwhile
		header.MessageType = goptp.DelayRespMsgType
		header.MessageLength = goptp.HeaderLen + goptp.DelayRespPayloadLen
		packet, _ = (&goptp.DelRespMsg{Header: header}).MarshalBinary()
		received := s.now() - int64(2*time.Millisecond)
		secs := uint64(received / int64(time.Second))
		packet[34], packet[35] = byte(secs>>40), byte(secs>>32)
		binary.BigEndian.PutUint32(packet[36:40], uint32(secs))
		binary.BigEndian.PutUint32(packet[40:44], uint32(received%int64(time.Second)))
		_, _ = conn.WriteToUDP(packet, s.ptpTo[1])
		time.Sleep(120 * time.Millisecond)
	}
}

// connect sets up the session, as a sender does when it is selected.
func (s *sender) connect() {
	if s.ptpTo != nil {
		go s.runPTP()
	}
	s.doPlist("SETUP", map[string]interface{}{"name": s.name, "timingProtocol": "PTP"})
	s.do("RECORD", "", nil)
	if !s.realtime {
		resp := s.doPlist("SETUP", map[string]interface{}{"streams": []interface{}{map[string]interface{}{
			"type": 103, "audioFormat": 0x400000, "spf": 1024, "ct": 3, "shk": s.key}}})
		port := streamPorts(s.t, resp.Body)[0]
		conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			s.t.Fatal(err)
		}
		s.audio = conn
	}
	s.mu.Lock()
	s.wake = make(chan struct{})
	s.mu.Unlock()
	go s.send()
}

func streamPorts(t *testing.T, body []byte) []int {
	var setup struct {
		Streams []struct {
			DataPort    uint16 `plist:"dataPort"`
			ControlPort uint16 `plist:"controlPort"`
		} `plist:"streams"`
	}
	if _, err := plist.Unmarshal(body, &setup); err != nil || len(setup.Streams) != 1 {
		t.Errorf("stream setup reply: %v", err)
		return []int{0, 0}
	}
	return []int{int(setup.Streams[0].DataPort), int(setup.Streams[0].ControlPort)}
}

// play starts or resumes playback, as the sender's play button does.
func (s *sender) play() {
	s.mu.Lock()
	k := s.next
	s.mu.Unlock()
	base := s.now() + int64(500*time.Millisecond)
	if s.realtime {
		// A realtime sender sets its stream up again when it resumes.
		resp := s.doPlist("SETUP", map[string]interface{}{"streams": []interface{}{map[string]interface{}{
			"type": 96, "audioFormat": 0x40000, "spf": 352, "ct": 2, "shk": s.key}}})
		ports := streamPorts(s.t, resp.Body)
		data, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ports[0]})
		if err != nil {
			s.t.Error(err)
			return
		}
		control, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ports[1]})
		if err != nil {
			s.t.Error(err)
			return
		}
		s.do("FLUSH", "", nil)
		s.mu.Lock()
		if s.data != nil {
			s.data.Close()
			s.control.Close()
		}
		s.data, s.control = data, control
		s.mu.Unlock()
	} else {
		if k > 0 {
			// Drop what was sent ahead before the pause.
			s.doPlist("FLUSHBUFFERED", map[string]interface{}{"flushUntilSeq": uint64(k - 1)})
		}
		secs, frac := ptpTime(base)
		s.doPlist("SETRATEANCHORTIME", map[string]interface{}{"rate": 1, "rtpTime": s.tsBase + uint32(k*s.samples),
			"networkTimeSecs": secs, "networkTimeFrac": frac, "networkTimeTimelineID": s.clockID})
	}
	s.mu.Lock()
	s.playIndex, s.playBase = k, base
	if !s.playing {
		s.playing = true
		close(s.wake)
	}
	s.mu.Unlock()
	if s.realtime {
		s.sendAnchor()
	}
}

// pause stops playback, as the sender does when its user pauses or when
// it obeys the receiver's pause command.
func (s *sender) pause() {
	s.mu.Lock()
	if s.playing {
		s.playing = false
		s.wake = make(chan struct{})
	}
	s.mu.Unlock()
	if s.realtime {
		s.doPlist("TEARDOWN", map[string]interface{}{"streams": []interface{}{map[string]interface{}{"type": 96}}})
	} else {
		s.doPlist("SETRATEANCHORTIME", map[string]interface{}{"rate": 0})
	}
}

func (s *sender) sendAnchor() {
	s.mu.Lock()
	control, k, base := s.control, s.playIndex, s.playBase
	s.mu.Unlock()
	if control == nil {
		return
	}
	// Sample F plays 11025 samples (250 ms) after the network time.
	packet := make([]byte, 28)
	packet[0], packet[1] = 0x90, 0xd7
	frame := s.tsBase + uint32(k*s.samples)
	binary.BigEndian.PutUint32(packet[4:8], frame)
	binary.BigEndian.PutUint64(packet[8:16], uint64(base-int64(250*time.Millisecond)))
	binary.BigEndian.PutUint32(packet[16:20], frame)
	binary.BigEndian.PutUint64(packet[20:28], s.clockID)
	_, _ = control.Write(packet)
}

// send writes packets in real time, about 300 ms before they play, while
// the sender is playing. A buffered stream that the receiver stops reading
// blocks here, as a TCP sender would.
func (s *sender) send() {
	lastAnchor := time.Now()
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		playing, wake := s.playing, s.wake
		k, index, base := s.next, s.playIndex, s.playBase
		data := s.data
		s.mu.Unlock()
		if !playing {
			select {
			case <-wake:
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		due := base + int64(k-index)*int64(s.samples)*int64(time.Second)/44100 - int64(300*time.Millisecond)
		if wait := time.Duration(due - s.now()); wait > 0 {
			time.Sleep(min(wait, 20*time.Millisecond))
			continue
		}
		var err error
		if s.realtime {
			if data != nil {
				_, err = data.Write(s.realtimePacket(k))
			}
			if time.Since(lastAnchor) > time.Second {
				s.sendAnchor()
				lastAnchor = time.Now()
			}
		} else {
			packet := s.bufferedPacket(k)
			if err = binary.Write(s.audio, binary.BigEndian, uint16(len(packet)+2)); err == nil {
				_, err = s.audio.Write(packet)
			}
		}
		if err != nil && !s.realtime {
			return
		}
		s.mu.Lock()
		if s.next == k {
			s.next = k + 1
		}
		s.mu.Unlock()
	}
}

func (s *sender) bufferedPacket(k int) []byte {
	header := make([]byte, 12)
	header[0] = 0x80
	header[1], header[2], header[3] = byte(k>>16), byte(k>>8), byte(k) // 24-bit sequence
	binary.BigEndian.PutUint32(header[4:8], s.tsBase+uint32(k*s.samples))
	binary.BigEndian.PutUint32(header[8:12], 0x12345678)
	nonce := make([]byte, 8)
	binary.BigEndian.PutUint64(nonce, uint64(k))
	ciphertext, tag, err := chacha20poly1305.EncryptAndSeal(s.key, nonce, s.aac[k%len(s.aac)], header[4:12])
	if err != nil {
		s.t.Error(err)
	}
	return append(append(append(header, ciphertext...), tag[:]...), nonce...)
}

// realtimePacket is an RTP packet carrying an uncompressed (escape) ALAC
// frame of a square wave.
func (s *sender) realtimePacket(k int) []byte {
	bits := newBitWriter()
	bits.put(1, 3)  // channel pair element
	bits.put(0, 4)  // instance tag
	bits.put(0, 12) // unused
	bits.put(1, 4)  // not partial, no shift, escape (uncompressed)
	for i := 0; i < s.samples; i++ {
		level := int16(8000)
		if (k*s.samples+i)/50%2 == 1 {
			level = -level
		}
		value := uint64(uint16(level))
		bits.put(value, 16)
		bits.put(value, 16)
	}
	bits.put(7, 3) // end
	header := make([]byte, 12)
	header[0], header[1] = 0x80, 96
	binary.BigEndian.PutUint16(header[2:4], uint16(k))
	binary.BigEndian.PutUint32(header[4:8], s.tsBase+uint32(k*s.samples))
	binary.BigEndian.PutUint32(header[8:12], 0x12345678)
	nonce := make([]byte, 8)
	binary.BigEndian.PutUint64(nonce, uint64(k))
	ciphertext, tag, err := chacha20poly1305.EncryptAndSeal(s.key, nonce, bits.bytes(), header[4:12])
	if err != nil {
		s.t.Error(err)
	}
	return append(append(append(header, ciphertext...), tag[:]...), nonce...)
}

func (s *sender) close() {
	s.mu.Lock()
	s.closed = true
	data, control := s.data, s.control
	s.mu.Unlock()
	if s.audio != nil {
		s.audio.Close()
	}
	if data != nil {
		data.Close()
		control.Close()
	}
	s.peer.Close()
	s.r.OnConnClose(s.conn)
}

type bitWriter struct {
	buf   []byte
	nbits int
}

func newBitWriter() *bitWriter { return &bitWriter{} }

func (w *bitWriter) put(value uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.nbits%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if value>>uint(i)&1 == 1 {
			w.buf[len(w.buf)-1] |= 0x80 >> uint(w.nbits%8)
		}
		w.nbits++
	}
}

func (w *bitWriter) bytes() []byte { return w.buf }

// ---- test setup ----

type rig struct {
	t      *testing.T
	r      *Rstp
	clock  *ptp.VirtualClock
	output *fakeOutput
	player *audio.Player
	aac    [][]byte
	ln     net.Listener
	ptpTo  []*net.UDPAddr // receiver PTP ports when senders run their own clocks
}

// newPTPRig gives every sender its own PTP clock, hours apart, followed by
// the real PTP server, as with real devices.
func newPTPRig(t *testing.T) *rig {
	g := newRig(t, true)
	g.clock.Reset() // not synchronized until PTP says so
	var conns []*net.UDPConn
	for i := 0; i < 2; i++ {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		g.ptpTo = append(g.ptpTo, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: conn.LocalAddr().(*net.UDPAddr).Port})
	}
	server := ptp.NewServerWithConns(g.clock, conns[0], conns[1])
	go server.Serve()
	t.Cleanup(server.Close)
	g.r.ClockSources = server.SetSources
	g.r.TimingPeers = server.SetPeers
	g.r.AnchorClock = server.PreferClock
	g.player.OnAnchorClock = server.PreferClock
	return g
}

func newRig(t *testing.T, synchronized bool) *rig {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if globals.ErrLog == nil {
		globals.ErrLog = log.New(os.Stderr, "", log.LstdFlags)
	}
	if homekit.Server == nil {
		server, err := homekit.NewServer("00:11:22:33:44:55", "test", t.TempDir(), []string{"127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		homekit.Server = server
	}
	if homekit.Device == nil {
		homekit.Device = homekit.NewAccessory("00:11:22:33:44:55", "00000000-0000-0000-0000-000000000001", globals.Features{})
	}
	clock := ptp.NewVirtualClock(0)
	clock.Step(0) // both simulated senders use the receiver's own timebase
	output := &fakeOutput{}
	player := audio.NewPlayerWithOutput(clock, audio.NewRing(64), output)
	player.SetSynchronized(synchronized)
	if err := player.Init(); err != nil {
		t.Fatal(err)
	}
	go player.Run()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &rig{t: t, clock: clock, output: output, player: player, ln: ln, aac: loadAAC(t, root)}
	g.r = &Rstp{
		streams:   map[string]*audio.Server{},
		owners:    map[string]*rtsp.Conn{},
		transient: nil,
		sessions:  map[*rtsp.Conn]bool{},
		classic:   map[*rtsp.Conn]*classicSession{},
		auth:      newPasswordAuth(""),
		player:    player,
	}
	t.Cleanup(func() {
		ln.Close()
		output.Stop()
	})
	return g
}

func loadAAC(t *testing.T, root string) [][]byte {
	file, err := os.Open(filepath.Join(root, "audio", "testdata", "sine-aac-lc.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var packets [][]byte
	for {
		var size uint16
		if err := binary.Read(file, binary.BigEndian, &size); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		packet := make([]byte, size)
		if _, err := io.ReadFull(file, packet); err != nil {
			t.Fatal(err)
		}
		packets = append(packets, packet)
	}
	return packets
}

// newSender connects a simulated device from its own loopback address.
func (g *rig) newSender(name string, ip net.IP, realtime bool, tsBase uint32) *sender {
	conn, peer := tcpPairFrom(g.t, g.ln, ip)
	s := &sender{t: g.t, r: g.r, clock: g.clock, conn: conn, peer: peer, name: name,
		path: "rtsp://receiver/" + name, key: make([]byte, 32), tsBase: tsBase,
		clockID: uint64(tsBase), realtime: realtime, aac: g.aac, samples: 1024, ip: ip}
	if g.ptpTo != nil {
		// Hours apart, like two devices' uptimes.
		s.ptpTo = g.ptpTo
		s.ahead = time.Duration(tsBase>>28) * time.Hour
	}
	for i := range s.key {
		s.key[i] = byte(i) ^ byte(tsBase>>24)
	}
	if realtime {
		s.samples = 352
	}
	g.t.Cleanup(s.close)
	return s
}

// activeStreams reports the senders whose streams may use the player.
func (g *rig) activeStreams() []string {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	var names []string
	for path, stream := range g.r.streams {
		if stream.Active() {
			names = append(names, g.r.sender(g.r.owners[path]).name+"@"+path)
		}
	}
	return names
}

func (g *rig) expectActive(s *sender) {
	g.t.Helper()
	g.r.mu.Lock()
	active, name := g.r.active, g.r.sender(g.r.active).name
	g.r.mu.Unlock()
	if active != s.conn {
		g.t.Fatalf("active sender is %q, want %q", name, s.name)
	}
	for _, name := range g.activeStreams() {
		if name != s.name+"@"+s.path {
			g.t.Fatalf("stream %s may use the player while %s plays", name, s.name)
		}
	}
}

// ---- tests ----

// The reported case: A plays, B connects and later starts playing. A must
// keep playing until then, and B must be heard afterwards.
func testSwitch(t *testing.T, aRealtime, bRealtime, synchronized bool) {
	testSwitchOn(t, newRig(t, synchronized), aRealtime, bRealtime)
}

func testSwitchOn(t *testing.T, g *rig, aRealtime, bRealtime bool) {
	a := g.newSender("A", net.IPv4(127, 0, 0, 1), aRealtime, 0x10000000)
	b := g.newSender("B", net.IPv4(127, 0, 0, 2), bRealtime, 0x50000000)

	a.connect()
	start := time.Now()
	a.play()
	expectSound(t, g.output, start, "A playing")

	b.connect()
	g.expectActive(a)
	expectSound(t, g.output, time.Now(), "A still playing after B connected")

	start = time.Now()
	b.play() // A ignores the pause command (no event channel here) and keeps sending
	g.expectActive(b)
	expectSound(t, g.output, start, "B playing after it took over")

	a.pause() // A obeys the pause, late
	g.expectActive(b)
	expectSound(t, g.output, time.Now(), "B unaffected by A's pause")

	start = time.Now()
	a.play()
	g.expectActive(a)
	expectSound(t, g.output, start, "A playing again")

	b.pause()
	start = time.Now()
	b.play()
	g.expectActive(b)
	expectSound(t, g.output, start, "B playing again")

	// B leaves: A gets the output back and plays when it resumes.
	b.close()
	g.expectActive(a)
	a.pause()
	start = time.Now()
	a.play()
	expectSound(t, g.output, start, "A after B disconnected")
}

func TestSwitchBufferedSenders(t *testing.T)  { testSwitch(t, false, false, true) }
func TestSwitchRealtimeSenders(t *testing.T)  { testSwitch(t, true, true, true) }
func TestSwitchMixedSenders(t *testing.T)     { testSwitch(t, false, true, true) }
func TestSwitchLowLatencyMixed(t *testing.T)  { testSwitch(t, true, false, false) }
func TestSwitchLowLatencyBuffered(t *testing.T) { testSwitch(t, false, false, false) }

// The reported failure: two devices with their own PTP clocks. B's audio
// was timed against A's clock after the switch and never played.
func TestSwitchRealtimeSendersWithOwnClocks(t *testing.T) {
	testSwitchOn(t, newPTPRig(t), true, true)
}

func TestSwitchBufferedSendersWithOwnClocks(t *testing.T) {
	testSwitchOn(t, newPTPRig(t), false, false)
}

// Both senders press play and pause at random moments from their own
// goroutines, as two RTSP connections would. Afterwards the receiver must
// be consistent and the last sender to play must be heard.
// The reported iPad case: the realtime sender pauses on the command but its
// stream keeps sending packets, so pressing play on it later sends no
// SETUP, only a progress update or a FLUSH. Either must switch back to it.
func TestSwitchBackToPausedRealtimeSender(t *testing.T) {
	g := newPTPRig(t)
	a := g.newSender("A", net.IPv4(127, 0, 0, 1), true, 0x10000000)
	b := g.newSender("B", net.IPv4(127, 0, 0, 2), true, 0x50000000)
	a.connect()
	start := time.Now()
	a.play()
	expectSound(t, g.output, start, "A playing")
	b.connect()

	start = time.Now()
	b.play() // A keeps streaming: it ignores the pause
	switched := time.Now()
	a.do("SET_PARAMETER", "text/parameters", []byte("progress: 1/2/3\r\n")) // reaction to the pause
	g.expectActive(b)
	expectSound(t, g.output, start, "B playing after it took over")

	time.Sleep(time.Until(switched.Add(resumeGrace + 100*time.Millisecond)))
	start = time.Now()
	a.do("SET_PARAMETER", "text/parameters", []byte("progress: 1/2/3\r\n")) // the user presses play on A
	g.expectActive(a)
	expectSound(t, g.output, start, "A after its progress update")

	time.Sleep(resumeGrace + 100*time.Millisecond)
	start = time.Now()
	b.do("FLUSH", "", nil) // B never stopped either; its user presses play
	g.expectActive(b)
	expectSound(t, g.output, start, "B after its FLUSH")
}

func TestSwitchStress(t *testing.T)              { stress(t, newRig(t, true)) }
func TestSwitchStressLowLatency(t *testing.T)     { stress(t, newRig(t, false)) }
func TestSwitchStressWithOwnClocks(t *testing.T)  { stress(t, newPTPRig(t)) }

func stress(t *testing.T, g *rig) {
	a := g.newSender("A", net.IPv4(127, 0, 0, 1), true, 0x10000000)
	b := g.newSender("B", net.IPv4(127, 0, 0, 2), false, 0x50000000)
	a.connect()
	b.connect()
	var wg sync.WaitGroup
	for i, s := range []*sender{a, b} {
		wg.Add(1)
		go func(s *sender, seed int64) {
			defer wg.Done()
			random := rand.New(rand.NewSource(seed))
			for n := 0; n < 40; n++ {
				if random.Intn(3) == 0 {
					s.pause()
				} else {
					s.play()
				}
				time.Sleep(time.Duration(random.Intn(60)) * time.Millisecond)
			}
		}(s, int64(i+1))
	}
	wg.Wait()
	for _, last := range []*sender{b, a, b} {
		start := time.Now()
		last.play()
		g.expectActive(last)
		expectSound(t, g.output, start, last.name+" after the storm")
	}
}
