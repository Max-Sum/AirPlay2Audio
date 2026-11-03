package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/brutella/hc/crypto/chacha20poly1305"
	"github.com/Max-Sum/AirPlay2Audio/globals"

	alac "github.com/mycophonic/saprobe-alac"
	"github.com/pion/rtp"
)

// SetupRealtime handles the AP2 type-96 stereo ALAC path in the given format.
// The normal buffered type-103 path stays separate and continues to use TCP.
func (s *Server) SetupRealtime(sharedKey []byte, samplesPerPacket uint32, format RealtimeFormat) (int, int, error) {
	decoder, err := alac.NewPacketDecoder(alac.PacketConfig{
		FrameLength: samplesPerPacket, BitDepth: uint8(format.Bits), NumChannels: 2,
		PB: 40, MB: 10, KB: 14, MaxRun: 255, SampleRate: uint32(format.Rate),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("initialize ALAC decoder: %w", err)
	}
	data, control, err := listenPair()
	if err != nil {
		return 0, 0, err
	}
	key := append([]byte(nil), sharedKey...)
	done, finish := s.startUDP(data, control, nil, samplesPerPacket, format.Rate, "AUDIO realtime connection closed")
	s.describe("realtime", "alac")
	s.noteFormat(format.Rate, format.Bits)
	link := &realtimeControlLink{conn: control, resent: make(chan realtimeDatagram, 64)}
	decode := func(raw []byte) (*PCMFrame, error) { return decodeRealtimePacket(decoder, key, raw, format) }
	go s.receiveRealtime(data, decode, link, done, finish)
	go s.receiveRealtimeControl(control, link, format.Rate, done, finish)
	log.Printf("AUDIO realtime ALAC ready: %d Hz %d-bit", format.Rate, format.Bits)
	return data.LocalAddr().(*net.UDPAddr).Port, control.LocalAddr().(*net.UDPAddr).Port, nil
}

func decodeRealtimePacket(decoder *alac.PacketDecoder, key, raw []byte, format RealtimeFormat) (*PCMFrame, error) {
	if len(raw) < 36 {
		return nil, fmt.Errorf("short realtime audio packet")
	}
	var packet rtp.Packet
	if err := packet.Unmarshal(raw); err != nil {
		return nil, err
	}
	if packet.PayloadType != 96 || len(packet.Payload) < 24 {
		return nil, fmt.Errorf("invalid realtime RTP payload")
	}
	payload := packet.Payload
	var tag [16]byte
	copy(tag[:], payload[len(payload)-24:len(payload)-8])
	plaintext, err := chacha20poly1305.DecryptAndVerify(key, payload[len(payload)-8:], payload[:len(payload)-24], tag, raw[4:12])
	if err != nil {
		return nil, err
	}
	if len(plaintext) == 0 {
		return nil, nil
	}
	pcm, err := decoder.DecodePacket(plaintext)
	if err != nil {
		return nil, err
	}
	samples := pcm16(pcm)
	if format.Bits == 24 {
		samples = pcm24(pcm)
	}
	return &PCMFrame{TcpPacket: TcpPacket{Packet: packet, SequenceNumber: uint32(packet.SequenceNumber)}, samples: samples, format: format.codec()}, nil
}

// Resend timing. In synchronized mode packets arrive a few hundred
// milliseconds before playback, so a gap can wait long enough for a resend
// round trip and up to two retries. Low-latency mode keeps its short wait;
// a resend there only helps on a fast network.
const (
	syncGapWait       = 120 * time.Millisecond
	syncResendRetry   = 40 * time.Millisecond
	lowLatencyGapWait = 8 * time.Millisecond
	maxResendRequests = 3
)

type netSnapshot struct{ received, missing, late, requests, recovered, full uint64 }

// realtimeDatagram is one received packet, tagged with the FLUSH epoch at
// the moment it was read so data from before a FLUSH can be dropped.
type realtimeDatagram struct {
	data  []byte
	epoch uint64
}

// realtimeControlLink connects the control channel to the audio loop: it
// carries resent packets in, and resend requests out to the sender's
// control address, which is learned from the first control packet.
type realtimeControlLink struct {
	conn   *net.UDPConn
	remote atomic.Pointer[net.UDPAddr]
	resent chan realtimeDatagram
	seq    uint16
}

// requestResend sends AirPlay 2's resend request: 0x80 0xD5, a request
// sequence number, the first missing sequence number and the count. The
// sender answers on the control channel with 0xD6 packets.
func (l *realtimeControlLink) requestResend(first, count uint16) bool {
	remote := l.remote.Load()
	if remote == nil {
		return false
	}
	l.seq++
	request := make([]byte, 8)
	request[0], request[1] = 0x80, 0xd5
	binary.BigEndian.PutUint16(request[2:4], l.seq)
	binary.BigEndian.PutUint16(request[4:6], first)
	binary.BigEndian.PutUint16(request[6:8], count)
	_, err := l.conn.WriteToUDP(request, remote)
	return err == nil
}

func (s *Server) receiveRealtime(conn *net.UDPConn, decodePacket func([]byte) (*PCMFrame, error), link *realtimeControlLink, done <-chan struct{}, finish func()) {
	defer finish()
	packets := make(chan realtimeDatagram, 256)
	var datagrams atomic.Uint64 // diagnostics: everything that arrived
	go func() {
		defer close(packets)
		buffer := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if datagrams.Add(1) == 1 {
				log.Printf("AUDIO realtime first audio packet from %s (%d bytes, active=%t)", from, n, s.Active())
			}
			s.touch()
			datagram := realtimeDatagram{data: append([]byte(nil), buffer[:n]...), epoch: s.flushEpoch.Load()}
			select {
			case packets <- datagram:
			case <-done:
				return
			}
		}
	}()
	reported := false
	reorder := newRealtimeReorder(syncGapWait)
	reorder.retry, reorder.maxRequests = syncResendRetry, maxResendRequests
	if !s.player.synchronized {
		reorder.wait = lowLatencyGapWait
		reorder.retry, reorder.maxRequests = lowLatencyGapWait, 1
	}
	var unsent uint64
	reorder.request = func(first, count uint16) {
		if !link.requestResend(first, count) {
			unsent += uint64(count)
		}
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	epoch := s.flushEpoch.Load()
	var full uint64
	var received, samples uint64
	lastReport := time.Now()
	lastLeadReport := time.Now()
	var minLead, maxLead time.Duration
	haveLead := false
	var prev netSnapshot
	lastStats := time.Now()
	// Diagnostics for a stream that plays nothing.
	var flushDropped, empty uint64
	noAudioCheck := time.After(5 * time.Second)
	decodedTotal := uint64(0)
	var standby standbyWatch
	// Bytes around the audio in a datagram: the RTP header, plus for
	// AirPlay 2 the nonce and authentication tag.
	overhead := 36
	s.mu.Lock()
	if s.gate.classic {
		overhead = 12
	}
	s.mu.Unlock()
	emit := func(frame *PCMFrame) {
		s.mu.Lock()
		defer s.mu.Unlock()
		select {
		case <-done:
			return
		default:
		}
		// A FLUSH can arrive while a packet is decoded or reordered. Do not
		// enqueue data from before that FLUSH into the newly emptied player.
		if s.flushEpoch.Load() != epoch {
			return
		}
		if !s.Active() {
			return // on standby: another sender is playing
		}
		if s.player.ringBuffer.TryPush(frame) != nil {
			full++ // Never block the UDP reader behind a full output queue.
		}
	}
	for {
		if current := s.flushEpoch.Load(); current != epoch {
			epoch = current
			reorder.reset()
		}
		var expired <-chan time.Time
		if at, ok := reorder.deadline(); ok {
			timer.Reset(time.Until(at))
			expired = timer.C
		}
		var datagram realtimeDatagram
		select {
		case <-done:
			return
		case <-expired:
			reorder.drain(time.Now(), false, emit)
			continue
		case <-noAudioCheck:
			if decodedTotal == 0 {
				log.Printf("AUDIO realtime no audio decoded 5 s after setup: datagrams=%d dropped-by-flush=%d empty=%d active=%t",
					datagrams.Load(), flushDropped, empty, s.Active())
			}
			continue
		case received, ok := <-packets:
			if !ok {
				return
			}
			datagram = received
		case datagram = <-link.resent:
		}
		if datagram.epoch != epoch || s.flushEpoch.Load() != epoch {
			// The datagram may have been read concurrently with FLUSH; drop it
			// rather than letting the old stream repopulate the output queue.
			flushDropped++
			continue
		}
		frame, err := decodePacket(datagram.data)
		if err != nil {
			if !reported {
				log.Printf("AUDIO realtime packet error: %v", err)
				reported = true
			}
			continue
		}
		if frame == nil {
			empty++
		}
		if frame != nil && !s.Active() {
			// Another sender owns the output: only watch for this sender
			// to start playing again, and keep it out of the statistics.
			decodedTotal++
			standby.observe(s, frame)
			reorder.push(frame, time.Now(), emit)
			continue
		}
		standby.reset()
		if frame != nil {
			received++
			decodedTotal++
			s.addBytes(len(datagram.data) - overhead)
			samples += uint64(frame.frames())
			if lead, ok := s.player.ArrivalLead(frame.Timestamp); ok {
				if !haveLead || lead < minLead {
					minLead = lead
				}
				if !haveLead || lead > maxLead {
					maxLead = lead
				}
				haveLead = true
			}
			reorder.push(frame, time.Now(), emit)
		}
		if time.Since(lastStats) >= time.Second {
			// Counters since the last hand-over; the 10 s report below resets
			// the totals, so the snapshot is reset with them.
			s.player.AddNetwork(received-prev.received, reorder.missing-prev.missing, reorder.late-prev.late,
				reorder.requests-prev.requests, reorder.recovered-prev.recovered, full-prev.full)
			prev = netSnapshot{received, reorder.missing, reorder.late, reorder.requests, reorder.recovered, full}
			lastStats = time.Now()
		}
		if haveLead && time.Since(lastLeadReport) >= 5*time.Second {
			log.Printf("AUDIO realtime arrival lead min=%d ms max=%d ms (time before scheduled playback)", minLead.Milliseconds(), maxLead.Milliseconds())
			haveLead, lastLeadReport = false, time.Now()
		}
		if time.Since(lastReport) >= 10*time.Second && (reorder.missing != 0 || reorder.late != 0 || full != 0 || reorder.requests != 0) {
			log.Printf("AUDIO realtime packets missing=%d late-or-duplicate=%d resend-requested=%d resend-recovered=%d resend-unsent=%d output-queue-full=%d received=%d decoded-samples=%d period=%s queue=%d/%d", reorder.missing, reorder.late, reorder.requests, reorder.recovered, unsent, full, received, samples, time.Since(lastReport).Round(time.Millisecond), s.player.ringBuffer.Length(), s.player.ringBuffer.Capacity())
			s.player.AddNetwork(received-prev.received, reorder.missing-prev.missing, reorder.late-prev.late,
				reorder.requests-prev.requests, reorder.recovered-prev.recovered, full-prev.full)
			reorder.missing, reorder.late, full = 0, 0, 0
			reorder.requests, reorder.recovered, unsent = 0, 0, 0
			received, samples = 0, 0
			prev, lastStats = netSnapshot{}, time.Now()
			lastReport = time.Now()
		}
	}
}

// Control channel rules from shairport-sync (rtp_ap2_control_receiver):
// right after the socket opens, a packet without the sentinel bit (0x10 in
// the first byte) is left over from the previous session on this port and is
// dropped; and when no control packet arrives for 7 seconds the anchor is
// forgotten, so playback waits for a fresh one instead of running on a stale
// one.
const (
	controlLeftoverWindow = 2 * time.Millisecond
	controlSilenceReset   = 7 * time.Second
)

// leftoverControlPacket reports whether a control packet received this soon
// after the socket opened belongs to a previous session.
func leftoverControlPacket(packet []byte, sinceStart time.Duration) bool {
	return len(packet) >= 28 && sinceStart < controlLeftoverWindow && packet[0]&0x10 == 0
}

// forgetAnchor drops this stream's anchor. Only the stream that owns the
// player clears the player's anchor; a stream on standby (another sender is
// playing) only forgets its own, so it cannot stop the other sender.
func (s *Server) forgetAnchor() bool {
	s.mu.Lock()
	had := s.gate.haveAnchor
	s.gate.haveAnchor = false
	s.mu.Unlock()
	if had && s.Active() {
		s.player.ControlChannel <- globals.ControlMessage{MType: globals.UNANCHOR}
	}
	return had
}

func (s *Server) receiveRealtimeControl(conn *net.UDPConn, link *realtimeControlLink, rate int, done <-chan struct{}, finish func()) {
	defer finish()
	buffer := make([]byte, 4096)
	announced := false
	seenTypes := map[byte]bool{}
	start := time.Now()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(controlSilenceReset))
		n, from, err := conn.ReadFromUDP(buffer)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				if s.forgetAnchor() {
					log.Printf("AUDIO realtime no control packets for %s; anchor reset (active=%t)", controlSilenceReset, s.Active())
				}
				announced = false
				continue
			}
			return
		}
		if leftoverControlPacket(buffer[:n], time.Since(start)) {
			log.Printf("AUDIO realtime dropped a control packet left over from a previous session (from %s)", from)
			continue
		}
		s.touch()
		if link.remote.Load() == nil {
			// Resend requests go back to where the sender's control packets
			// come from, as shairport-sync does.
			link.remote.Store(from)
		}
		if n >= 2 && !seenTypes[buffer[1]] && len(seenTypes) < 16 {
			// Diagnostics: which kinds of packets this sender uses here.
			seenTypes[buffer[1]] = true
			log.Printf("AUDIO realtime control packet type=0x%02x size=%d from %s", buffer[1], n, from)
		}
		// Resent audio: a 4-byte header, then the original RTP packet.
		if n >= 4+36 && buffer[1] == 0xd6 {
			datagram := realtimeDatagram{data: append([]byte(nil), buffer[4:n]...), epoch: s.flushEpoch.Load()}
			select {
			case link.resent <- datagram:
			case <-done:
				return
			default: // the audio loop is behind; this packet would be too late
			}
			continue
		}
		// PTP realtime anchor: frame_1 at byte 4, network nanoseconds at byte
		// 8, frame_2 at byte 16; see realtimeAnchorStamp for the modes.
		if n >= 28 && buffer[1] == 0xd7 {
			stamp := realtimeAnchorStamp(binary.BigEndian.Uint32(buffer[4:8]), binary.BigEndian.Uint32(buffer[16:20]), s.player.realtimeAnchor, rate)
			networkTime := int64(binary.BigEndian.Uint64(buffer[8:16]))
			// Bytes 20..28 name the PTP clock the network time is on.
			clockID := binary.BigEndian.Uint64(buffer[20:28])
			if s.Active() && s.player.OnAnchorClock != nil && clockID != 0 {
				s.player.OnAnchorClock(clockID)
			}
			s.anchor(networkTime, stamp)
			if !announced {
				// Bytes 16..20 hold the frame the network time refers to; the
				// difference is the sender's stream latency (normally 77175).
				latency := int32(binary.BigEndian.Uint32(buffer[16:20]) - binary.BigEndian.Uint32(buffer[4:8]))
				s.noteLatency(time.Duration(latency) * time.Second / time.Duration(rate))
				log.Printf("AUDIO realtime PTP anchor received; stream latency=%d samples (%d ms) clock=%016x", latency, int64(latency)*1000/int64(rate), clockID)
				announced = true
			}
		}
	}
}
