package ptp

import (
	"io"
	"log"
	"net"
	"testing"
	"time"

	ptp "github.com/albanseurat/go-ptp"
	"github.com/Max-Sum/AirPlay2Audio/globals"
)

// fakeMaster stands for one sender's PTP clock: its time is the receiver's
// local time plus ahead.
type fakeMaster struct {
	id    uint64
	ip    net.IP
	ahead time.Duration
	seq   uint16
}

// exchange runs one Sync, Follow_Up, Delay_Req, Delay_Resp round.
func (m *fakeMaster) exchange(t *testing.T, s *Server) {
	t.Helper()
	m.seq++
	m.exchangeVia(t, s, m.ip, m.seq, true)
}

// exchangeVia sends one round from ip with the given sequence number. An
// incomplete round has no Delay_Resp, as when the reply is lost.
func (m *fakeMaster) exchangeVia(t *testing.T, s *Server, ip net.IP, seq uint16, complete bool) {
	t.Helper()
	src := &net.UDPAddr{IP: ip, Port: 320}
	header := ptp.Header{VersionPTP: ptp.Version2, ClockIdentity: m.id, PortNumber: 1, SequenceID: seq}
	local := s.clock.LocalNow()
	header.MessageType = ptp.SyncMsgType
	s.handle(serverMsg{Msg: &ptp.SyncMsg{Header: header}, Src: src, LocalTimeStamp: local})
	header.MessageType = ptp.FollowUpMsgType
	s.handle(serverMsg{Msg: &ptp.FollowUpMsg{Header: header, PreciseOriginTimestamp: local.Add(m.ahead)}, Src: src})
	if !complete {
		return
	}
	header.MessageType = ptp.DelayRespMsgType
	s.handle(serverMsg{Msg: &ptp.DelRespMsg{Header: header, ReceiveTimestamp: s.clock.LocalNow().Add(m.ahead)}, Src: src})
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	if globals.ErrLog == nil {
		globals.ErrLog = log.New(io.Discard, "", 0)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	s := NewServer(NewVirtualClock(0))
	s.eventConn, s.genConn = conn, conn // Delay_Req goes to port 319 of a loopback address; nobody reads it
	return s
}

func assertFollows(t *testing.T, s *Server, m *fakeMaster, what string) {
	t.Helper()
	diff := s.clock.Now().Sub(s.clock.LocalNow().Add(m.ahead))
	if diff < -20*time.Millisecond || diff > 20*time.Millisecond {
		t.Fatalf("%s: clock is %v away from sender %x", what, diff, m.id)
	}
}

func TestSwitchingSendersFollowsNewClockAtOnce(t *testing.T) {
	s := newTestServer(t)
	a := &fakeMaster{id: 0xa, ip: net.IPv4(192, 0, 2, 10), ahead: time.Hour}
	b := &fakeMaster{id: 0xb, ip: net.IPv4(192, 0, 2, 20), ahead: -5 * time.Hour}
	s.SetPeers([]net.IP{a.ip, b.ip})
	s.SetSources([]net.IP{a.ip})
	for i := 0; i < 5; i++ {
		a.exchange(t, s)
		b.exchange(t, s) // B is connected but waiting: measured, not followed
	}
	assertFollows(t, s, a, "while A plays")

	// B starts playing: its clock was measured all along, so the switch
	// needs no new PTP packet.
	s.SetSources([]net.IP{b.ip})
	assertFollows(t, s, b, "right after switching to B")
	for i := 0; i < 3; i++ {
		a.exchange(t, s)
		b.exchange(t, s)
	}
	assertFollows(t, s, b, "while B plays")

	// And back.
	s.SetSources([]net.IP{a.ip})
	assertFollows(t, s, a, "after switching back to A")
}

func TestSwitchToUnmeasuredSenderStepsOnFirstMeasurement(t *testing.T) {
	s := newTestServer(t)
	a := &fakeMaster{id: 0xa, ip: net.IPv4(192, 0, 2, 10), ahead: time.Hour}
	b := &fakeMaster{id: 0xb, ip: net.IPv4(192, 0, 2, 20), ahead: -5 * time.Hour}
	s.SetPeers([]net.IP{a.ip, b.ip})
	s.SetSources([]net.IP{a.ip})
	for i := 0; i < 3; i++ {
		a.exchange(t, s)
	}
	s.SetSources([]net.IP{b.ip})
	b.exchange(t, s)
	// Without the immediate step, three hours of difference would need
	// several confirming measurements first.
	assertFollows(t, s, b, "after B's first measurement")
}

func TestAnchorClockWins(t *testing.T) {
	s := newTestServer(t)
	a := &fakeMaster{id: 0xa, ip: net.IPv4(192, 0, 2, 10), ahead: time.Hour}
	b := &fakeMaster{id: 0xb, ip: net.IPv4(192, 0, 2, 20), ahead: -5 * time.Hour}
	s.SetPeers([]net.IP{a.ip, b.ip})
	s.SetSources([]net.IP{b.ip})
	for i := 0; i < 3; i++ {
		a.exchange(t, s)
		b.exchange(t, s)
	}
	// B's anchors are on A's clock (A is the grandmaster for both).
	s.PreferClock(a.id)
	assertFollows(t, s, a, "anchor names A's clock")
	b.exchange(t, s)
	assertFollows(t, s, a, "B's packets must not move the clock")

	// The preference belongs to the sender that set it.
	s.SetSources([]net.IP{a.ip})
	s.SetSources([]net.IP{b.ip})
	assertFollows(t, s, b, "after the preference was cleared")
}

// Apple devices send the same clock over IPv4, IPv6 and link-local IPv6,
// with the same sequence numbers on each.
func TestOneClockOverSeveralAddresses(t *testing.T) {
	s := newTestServer(t)
	a := &fakeMaster{id: 0xa, ip: net.ParseIP("2001:db8::10"), ahead: time.Hour}
	v4, linkLocal := net.IPv4(192, 0, 2, 10), net.ParseIP("fe80::10")
	s.SetPeers([]net.IP{a.ip})
	s.SetSources([]net.IP{a.ip})
	for seq := uint16(1); seq <= 6; seq++ {
		a.exchangeVia(t, s, v4, seq, true)
		a.exchangeVia(t, s, a.ip, seq, true)
		a.exchangeVia(t, s, linkLocal, seq, true)
	}
	assertFollows(t, s, a, "same clock over three addresses")
}

// The reported iPhone case: the address the sender announced only carries
// Sync and Follow_Up, while complete exchanges happen over another address.
func TestClockMeasuredOverAnotherAddress(t *testing.T) {
	s := newTestServer(t)
	a := &fakeMaster{id: 0xa, ip: net.ParseIP("2001:db8::10"), ahead: time.Hour}
	b := &fakeMaster{id: 0xb, ip: net.ParseIP("2001:db8::20"), ahead: 5 * time.Hour}
	v4 := net.IPv4(192, 0, 2, 10)
	s.SetPeers([]net.IP{a.ip, b.ip})
	s.SetSources([]net.IP{b.ip})
	for i := 0; i < 3; i++ {
		b.exchange(t, s)
	}
	assertFollows(t, s, b, "B playing")
	s.SetSources([]net.IP{a.ip})
	for seq := uint16(1); seq <= 3; seq++ {
		a.exchangeVia(t, s, a.ip, seq, false)
		a.exchangeVia(t, s, v4, seq, true)
	}
	assertFollows(t, s, a, "A measured only over IPv4")
}

// An anchor may name a clock before any exchange with it has completed;
// the clock must switch as soon as it is measured.
func TestAnchorNamesClockBeforeItIsMeasured(t *testing.T) {
	s := newTestServer(t)
	a := &fakeMaster{id: 0xa, ip: net.IPv4(192, 0, 2, 10), ahead: time.Hour}
	b := &fakeMaster{id: 0xb, ip: net.IPv4(192, 0, 2, 20), ahead: 5 * time.Hour}
	s.SetPeers([]net.IP{a.ip, b.ip})
	s.SetSources([]net.IP{a.ip})
	for i := 0; i < 3; i++ {
		a.exchange(t, s)
	}
	s.SetSources([]net.IP{b.ip})
	s.PreferClock(b.id)
	b.exchange(t, s)
	assertFollows(t, s, b, "after B's first measurement")
}

func TestUnknownSendersAndAirPlay1AreIgnored(t *testing.T) {
	s := newTestServer(t)
	a := &fakeMaster{id: 0xa, ip: net.IPv4(192, 0, 2, 10), ahead: time.Hour}
	stranger := &fakeMaster{id: 0xc, ip: net.IPv4(192, 0, 2, 99), ahead: 7 * time.Hour}
	s.SetPeers([]net.IP{a.ip})
	s.SetSources([]net.IP{a.ip})
	for i := 0; i < 3; i++ {
		a.exchange(t, s)
		stranger.exchange(t, s)
	}
	assertFollows(t, s, a, "with an unrelated PTP master on the network")

	// AirPlay 1 sender: PTP must leave the clock alone.
	s.SetSources([]net.IP{})
	s.clock.Reset()
	for i := 0; i < 3; i++ {
		a.exchange(t, s)
	}
	if s.clock.Synchronized() {
		t.Fatal("PTP moved the clock while an AirPlay 1 sender plays")
	}
}
