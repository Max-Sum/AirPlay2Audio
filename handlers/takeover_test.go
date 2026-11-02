package handlers

import (
	"net"
	"testing"
	"time"

	"goplay2/audio"
	"goplay2/ptp"
	"goplay2/rtsp"
)

// tcpPair returns the receiver side of a loopback TCP connection and the
// sender side.
func tcpPair(t *testing.T, listener net.Listener) (*rtsp.Conn, net.Conn) {
	return tcpPairFrom(t, listener, nil)
}

// tcpPairFrom connects from the given local address, so tests can stand
// for two different devices.
func tcpPairFrom(t *testing.T, listener net.Listener, local net.IP) (*rtsp.Conn, net.Conn) {
	t.Helper()
	dialer := net.Dialer{}
	if local != nil {
		dialer.LocalAddr = &net.TCPAddr{IP: local}
	}
	sender, err := dialer.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Skipf("cannot connect from %v: %v", local, err)
	}
	receiver, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	conn := &rtsp.Conn{}
	conn.SetNetConn(receiver)
	return conn, sender
}

func assertOpen(t *testing.T, sender net.Conn) {
	t.Helper()
	_ = sender.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := sender.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("sender connection was closed: %v", err)
	}
}

func assertClosed(t *testing.T, sender net.Conn) {
	t.Helper()
	_ = sender.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := sender.Read(make([]byte, 1)); err == nil {
		t.Fatal("sender connection still open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("sender connection not closed")
	}
}

// drainControl empties the player's control channel so Suspend and Resume
// never block in tests (no Run loop reads it).
func drainControl(player *audio.Player) {
	for {
		select {
		case <-player.ControlChannel:
		default:
			return
		}
	}
}

func TestLaterSenderTakesOverWhenItPlays(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	first, firstSender := tcpPairFrom(t, listener, net.IPv4(127, 0, 0, 1))
	second, secondSender := tcpPairFrom(t, listener, net.IPv4(127, 0, 0, 2))
	defer firstSender.Close()
	defer secondSender.Close()
	defer first.Close()
	defer second.Close()

	player := audio.NewPlayer(ptp.NewVirtualClock(0), audio.NewRing(16))
	var sources []net.IP
	r := &Rstp{
		streams:      map[string]*audio.Server{},
		owners:       map[string]*rtsp.Conn{},
		sessions:     map[*rtsp.Conn]bool{},
		auth:         newPasswordAuth(""),
		player:       player,
		ClockSources: func(ips []net.IP) { sources = ips },
	}
	connect := func(conn *rtsp.Conn, path, name string) *audio.Server {
		stream, _ := audio.NewServer(player)
		r.admit(conn)
		if r.active != conn {
			stream.Standby()
		}
		r.streams[path], r.owners[path] = stream, conn
		r.sessionStarted(conn, name)
		return stream
	}

	a := connect(first, "a", "A")
	b := connect(second, "b", "B")
	// Both stay connected; A keeps the output until B plays.
	if r.active != first || !a.Active() || b.Active() {
		t.Fatal("second sender took the output before playing")
	}
	assertOpen(t, firstSender)
	if len(sources) != 1 || !sources[0].Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("clock not following the first sender: %v", sources)
	}

	r.startPlaying(second)
	drainControl(player)
	if r.active != second || a.Active() || !b.Active() {
		t.Fatal("playing sender did not take over")
	}
	if !r.sessions[first] || len(r.streams) != 2 {
		t.Fatal("previous sender's session was ended")
	}
	assertOpen(t, firstSender)
	if len(sources) != 1 || !sources[0].Equal(net.IPv4(127, 0, 0, 2)) {
		t.Fatalf("clock not following the new sender: %v", sources)
	}
	if r.foreground(first) || !r.foreground(second) {
		t.Fatal("paused sender may still change the output")
	}

	// Pressing play on A switches back.
	r.startPlaying(first)
	drainControl(player)
	if r.active != first || !a.Active() || b.Active() {
		t.Fatal("first sender could not take the output back")
	}

	// When the active sender leaves, the other one gets the output.
	r.OnConnClose(first)
	drainControl(player)
	if r.active != second || !b.Active() {
		t.Fatal("remaining sender did not get the output")
	}
}

func TestSameDeviceReconnectReplacesOldConnection(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	first, firstSender := tcpPair(t, listener)
	second, secondSender := tcpPair(t, listener)
	defer firstSender.Close()
	defer secondSender.Close()
	defer second.Close()

	player := audio.NewPlayer(ptp.NewVirtualClock(0), audio.NewRing(16))
	stream, _ := audio.NewServer(player)
	r := &Rstp{
		streams:  map[string]*audio.Server{"rtsp://receiver/first": stream},
		owners:   map[string]*rtsp.Conn{"rtsp://receiver/first": first},
		sessions: map[*rtsp.Conn]bool{first: true},
		active:   first,
		auth:     newPasswordAuth(""),
		player:   player,
	}
	r.admit(second)
	if len(r.streams) != 0 || r.sessions[first] || r.active != second {
		t.Fatal("old connection from the same device still registered")
	}
	assertClosed(t, firstSender)
	// The closed connection's late cleanup must not touch the new one.
	r.OnConnClose(first)
	if r.active != second {
		t.Fatal("cleanup of old connection cleared the new sender")
	}
}

func TestDisconnectStopsStream(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, sender := tcpPair(t, listener)
	defer sender.Close()
	player := audio.NewPlayer(ptp.NewVirtualClock(0), audio.NewRing(16))
	stream, _ := audio.NewServer(player)
	r := &Rstp{
		streams:  map[string]*audio.Server{"s": stream},
		owners:   map[string]*rtsp.Conn{"s": conn},
		sessions: map[*rtsp.Conn]bool{conn: true},
		active:   conn,
		auth:     newPasswordAuth(""),
		player:   player,
	}
	r.OnConnClose(conn)
	if len(r.streams) != 0 || r.active != nil {
		t.Fatal("stream of disconnected sender left running")
	}
}
