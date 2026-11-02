package ptp

import (
	"encoding"
	"fmt"
	"github.com/albanseurat/go-ptp"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/netutil"
	"net"
	"time"
)

// defaultClockIdentity is used until SetIdentity gives the receiver its own.
const defaultClockIdentity = 0x007343fffe123456

// ClockIdentityFromMAC forms a PTP clock identity from a 48-bit hardware
// address as IEEE 1588 suggests: the EUI-64 with FF FE inserted in the
// middle. Receivers then differ on a network, and on one computer when each
// instance has its own -device-id-offset.
func ClockIdentityFromMAC(mac net.HardwareAddr) uint64 {
	if len(mac) != 6 {
		return defaultClockIdentity
	}
	eui := []byte{mac[0], mac[1], mac[2], 0xff, 0xfe, mac[3], mac[4], mac[5]}
	var id uint64
	for _, b := range eui {
		id = id<<8 | uint64(b)
	}
	return id
}

type serverMsg struct {
	LocalTimeStamp time.Time
	Msg            interface{}
	Src            *net.UDPAddr
}

type Server struct {
	genConn      *net.UDPConn
	eventConn    *net.UDPConn
	genConn6     *net.UDPConn // nil without IPv6
	eventConn6   *net.UDPConn
	clock        *VirtualClock
	tracker      clockTracker // see clocks.go
	identity     uint64       // this receiver's clock identity in Delay_Req
}

func sameSources(a, b []net.IP) bool {
	contains := func(list []net.IP, ip net.IP) bool {
		for _, item := range list {
			if item.Equal(ip) {
				return true
			}
		}
		return false
	}
	for _, ip := range a {
		if !contains(b, ip) {
			return false
		}
	}
	for _, ip := range b {
		if !contains(a, ip) {
			return false
		}
	}
	return true
}

func NewServer(clock *VirtualClock) *Server {
	return &Server{clock: clock, identity: defaultClockIdentity}
}

// NewServerWithConns uses sockets the caller opened instead of UDP 319 and
// 320, so tests can run beside a receiver that holds those ports.
func NewServerWithConns(clock *VirtualClock, event, general *net.UDPConn) *Server {
	return &Server{clock: clock, eventConn: event, genConn: general, identity: defaultClockIdentity}
}

// SetIdentity sets the clock identity sent in Delay_Req. Call before Serve.
func (s *Server) SetIdentity(identity uint64) { s.identity = identity }

func NewDelayRequest(identity uint64, sequenceId uint16) *ptp.DelReqMsg {
	return &ptp.DelReqMsg{
		Header: ptp.Header{
			Flags: ptp.Flags{
				Unicast:   true,
				TwoSteps:  true,
				TimeScale: true,
			},
			MessageType:      ptp.DelayReqMsgType,
			MessageLength:    ptp.HeaderLen + ptp.DelayReqPayloadLen,
			VersionPTP:       ptp.Version2,
			ClockIdentity:    identity,
			PortNumber:       1,
			SequenceID:       sequenceId,
			LogMessagePeriod: 0x7f,
		},
		OriginTimestamp: time.Unix(0, 0),
	}

}

func (s *Server) Listen() error {
	var err error
	s.genConn, s.genConn6, err = netutil.ListenFixedUDP(320)
	if err != nil {
		return fmt.Errorf("PTP UDP 320: %w", err)
	}
	s.eventConn, s.eventConn6, err = netutil.ListenFixedUDP(319)
	if err != nil {
		s.Close()
		return fmt.Errorf("PTP UDP 319: %w", err)
	}
	if (s.genConn6 == nil) != (s.eventConn6 == nil) {
		// Use IPv6 only with both ports.
		if s.genConn6 != nil {
			s.genConn6.Close()
		}
		if s.eventConn6 != nil {
			s.eventConn6.Close()
		}
		s.genConn6, s.eventConn6 = nil, nil
	}
	return nil
}

func (s *Server) Close() {
	for _, conn := range []*net.UDPConn{s.genConn, s.eventConn, s.genConn6, s.eventConn6} {
		if conn != nil {
			conn.Close()
		}
	}
}

func (s *Server) Serve() {
	if s.genConn == nil || s.eventConn == nil {
		return
	}
	defer s.Close()
	packetChan := make(chan serverMsg)

	go handlePtpPacket(s.clock, s.genConn, packetChan)
	go handlePtpPacket(s.clock, s.eventConn, packetChan)
	if s.eventConn6 != nil {
		go handlePtpPacket(s.clock, s.genConn6, packetChan)
		go handlePtpPacket(s.clock, s.eventConn6, packetChan)
	}

	for {
		msg := <-packetChan
		if _, stop := msg.Msg.(bool); stop {
			return
		}
		if !s.handle(msg) {
			return
		}
	}
}

// handle processes one PTP message; it returns false when the socket failed.
func (s *Server) handle(msg serverMsg) bool {
	t := &s.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if msg.Src == nil {
		return true
	}
	var header *ptp.Header
	switch x := msg.Msg.(type) {
	case *ptp.AnnounceMsg:
		header = &x.Header
	case *ptp.SyncMsg:
		header = &x.Header
	case *ptp.FollowUpMsg:
		header = &x.Header
	case *ptp.DelRespMsg:
		header = &x.Header
	default:
		return true
	}
	now := time.Now()
	t.reportLocked(now)
	c := t.pathLocked(header.ClockIdentity, msg.Src.IP, now)
	if c == nil {
		return true
	}
	m := c.measures
	switch x := msg.Msg.(type) {
	case *ptp.AnnounceMsg:
		m.currentUTCoffset = time.Duration(x.CurrentUtcOffset) * time.Second
	case *ptp.SyncMsg:
		c.syncs++
		m.prune(x.SequenceID)
		m.addSync(x.SequenceID, msg.LocalTimeStamp)
	case *ptp.FollowUpMsg:
		c.followUps++
		m.addFollowUp(x.SequenceID, x.PreciseOriginTimestamp)
		binary, _ := NewDelayRequest(s.identity, x.SequenceID).MarshalBinary()
		binary[0] |= 0x10 // add transport specific to 1 for Apple ...
		destination := &net.UDPAddr{IP: msg.Src.IP, Port: 319, Zone: msg.Src.Zone}
		m.addDelayReq(x.SequenceID, s.clock.LocalNow())
		out := s.eventConn
		if msg.Src.IP.To4() == nil && s.eventConn6 != nil {
			out = s.eventConn6
		}
		if out == nil {
			return true
		}
		if _, err := out.WriteToUDP(binary, destination); err != nil {
			globals.ErrLog.Printf("Error writing UDP packet %v\n", err)
			return false
		}
	case *ptp.DelRespMsg:
		c.delayResps++
		m.addDelayResp(x.SequenceID, x.ReceiveTimestamp)
		if result, err := m.latest(); err == nil {
			first := !s.clock.Synchronized()
			t.measuredLocked(c, result.Offset, s.clock, now)
			if first && s.clock.Synchronized() {
				globals.ErrLog.Printf("PTP clock synchronized")
			}
		}
	}
	return true
}

func handlePtpPacket(clock *VirtualClock, conn *net.UDPConn, packetChan chan serverMsg) {
	var udpPacket [1024]byte
	var msg encoding.BinaryUnmarshaler
	defer conn.Close()
	defer func() {
		packetChan <- serverMsg{Msg: false}
	}()

	for {
		msgLen, addr, err := conn.ReadFromUDP(udpPacket[:])
		receivedTime := clock.LocalNow()
		if err != nil {
			globals.ErrLog.Printf("Error reading UDP packet %v\n", err)
			return
		}
		if msgLen < ptp.HeaderLen {
			continue
		}
		header := &ptp.Header{}
		if err = parsePacket(udpPacket[:ptp.HeaderLen], header); err != nil {
			continue
		}
		switch header.MessageType {
		case ptp.AnnounceMsgType:
			msg = &ptp.AnnounceMsg{}
		case ptp.SignalingMsgType:
			continue
		case ptp.SyncMsgType:
			msg = &ptp.SyncMsg{}
		case ptp.FollowUpMsgType:
			msg = &ptp.FollowUpMsg{}
			if msgLen < ptp.HeaderLen+ptp.FollowUpPayloadLen {
				continue
			}
			msgLen = ptp.HeaderLen + ptp.FollowUpPayloadLen
		case ptp.DelayRespMsgType:
			msg = &ptp.DelRespMsg{}
			if msgLen < ptp.HeaderLen+ptp.DelayRespPayloadLen {
				continue
			}
			msgLen = ptp.HeaderLen + ptp.DelayRespPayloadLen
		default:
			continue
		}
		if err := parsePacket(udpPacket[:msgLen], msg); err != nil {
			continue
		}
		packetChan <- serverMsg{Msg: msg, Src: addr, LocalTimeStamp: receivedTime}
	}
}

func parsePacket(packet []byte, unmarshaler encoding.BinaryUnmarshaler) error {
	if err := unmarshaler.UnmarshalBinary(packet); err != nil {
		globals.ErrLog.Printf("Error parsing PTP %v\n", err)
		return err
	}
	return nil
}
