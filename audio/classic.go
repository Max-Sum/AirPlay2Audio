package audio

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"time"

	"goplay2/netutil"
	"goplay2/ntp"

	alac "github.com/mycophonic/saprobe-alac"
	"github.com/pion/rtp"
)

// ClassicFormat describes an AirPlay 1 (RAOP) stream, from the sender's
// ANNOUNCE.
type ClassicFormat struct {
	// Codec is "alac" (Apple Lossless, from the rtpmap AppleLossless) or
	// "l16" (uncompressed big-endian 16-bit PCM).
	Codec string
	ALAC  alac.PacketConfig
	// Key and IV are the AES-128-CBC session key and IV. Key is nil for an
	// unencrypted stream.
	Key, IV []byte
}

// SamplesPerPacket is the frame length the sender announced.
func (f ClassicFormat) SamplesPerPacket() uint32 {
	if f.ALAC.FrameLength != 0 {
		return f.ALAC.FrameLength
	}
	return 352
}

// ClassicPeer is where the sender listens, from the SETUP Transport header.
type ClassicPeer struct {
	Addr        *net.UDPAddr // sender address; the port is ignored
	ControlPort int
	TimingPort  int
}

// classicLatencyAllowance is the latency the receiver adds to the sender's
// own, reported in RECORD's Audio-Latency (as shairport-sync does). With
// the usual 77175-sample sender latency, audio plays 2 s after it is sent.
const ClassicLatencyAllowance = 11025

// SetupClassic opens the data, control and timing sockets of an AirPlay 1
// session and starts following the sender's NTP clock.
func (s *Server) SetupClassic(format ClassicFormat, peer ClassicPeer) (int, int, int, error) {
	decode, err := classicDecoder(format)
	if err != nil {
		return 0, 0, 0, err
	}
	data, control, err := listenPair()
	if err != nil {
		return 0, 0, 0, err
	}
	timingConn, err := netutil.ListenUDP()
	if err != nil {
		data.Close()
		control.Close()
		return 0, 0, 0, err
	}
	// The virtual clock follows PTP for AirPlay 2 senders. An AirPlay 1
	// sender's clock has an unrelated origin, so start again from NTP.
	clock := s.player.clock.virtualClock
	if s.Active() {
		clock.Reset()
	}
	timing := ntp.Start(timingConn, &net.UDPAddr{IP: peer.Addr.IP, Zone: peer.Addr.Zone, Port: peer.TimingPort}, clock)
	done, finish := s.startUDP(data, control, timing, format.SamplesPerPacket(), "AUDIO AirPlay 1 connection closed")
	s.describe("classic", format.Codec)
	s.noteFormat(int(sampleRate), 16)
	link := &realtimeControlLink{conn: control, resent: make(chan realtimeDatagram, 64)}
	if peer.ControlPort > 0 {
		link.remote.Store(&net.UDPAddr{IP: peer.Addr.IP, Zone: peer.Addr.Zone, Port: peer.ControlPort})
	}
	go s.receiveRealtime(data, decode, link, done, finish)
	go s.receiveClassicControl(control, link, done, finish)
	log.Printf("AUDIO AirPlay 1 %s ready (encrypted=%t)", format.Codec, format.Key != nil)
	return data.LocalAddr().(*net.UDPAddr).Port, control.LocalAddr().(*net.UDPAddr).Port, timingConn.LocalAddr().(*net.UDPAddr).Port, nil
}

func classicDecoder(format ClassicFormat) (func([]byte) (*PCMFrame, error), error) {
	var block cipher.Block
	if format.Key != nil {
		if len(format.IV) != aes.BlockSize {
			return nil, fmt.Errorf("AES IV has %d bytes", len(format.IV))
		}
		var err error
		if block, err = aes.NewCipher(format.Key); err != nil {
			return nil, err
		}
	}
	iv := append([]byte(nil), format.IV...)
	switch format.Codec {
	case "alac":
		cfg := format.ALAC
		if cfg.BitDepth != 16 || cfg.NumChannels != 2 || cfg.SampleRate != uint32(sampleRate) {
			return nil, fmt.Errorf("unsupported ALAC format: %d-bit, %d channels, %d Hz", cfg.BitDepth, cfg.NumChannels, cfg.SampleRate)
		}
		decoder, err := alac.NewPacketDecoder(cfg)
		if err != nil {
			return nil, fmt.Errorf("initialize ALAC decoder: %w", err)
		}
		return func(raw []byte) (*PCMFrame, error) {
			return decodeClassicPacket(raw, block, iv, decoder.DecodePacket)
		}, nil
	case "l16":
		return func(raw []byte) (*PCMFrame, error) {
			return decodeClassicPacket(raw, block, iv, func(payload []byte) ([]byte, error) {
				pcm := make([]byte, len(payload)&^3)
				for i := 0; i+1 < len(pcm); i += 2 {
					pcm[i], pcm[i+1] = payload[i+1], payload[i]
				}
				return pcm, nil
			})
		}, nil
	}
	return nil, fmt.Errorf("unsupported AirPlay 1 codec %q", format.Codec)
}

// decodeClassicPacket decrypts and decodes one RTP audio packet. Only whole
// 16-byte blocks of the payload are encrypted, each packet starting again
// from the session IV; trailing bytes are sent in the clear.
func decodeClassicPacket(raw []byte, block cipher.Block, iv []byte, decode func([]byte) ([]byte, error)) (*PCMFrame, error) {
	if len(raw) < 12 || raw[1]&0x7f != 0x60 {
		return nil, fmt.Errorf("not an AirPlay 1 audio packet")
	}
	var packet rtp.Packet
	if err := packet.Unmarshal(raw); err != nil {
		return nil, err
	}
	payload := append([]byte(nil), packet.Payload...)
	if block != nil {
		n := len(payload) &^ (aes.BlockSize - 1)
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(payload[:n], payload[:n])
	}
	if len(payload) == 0 {
		return nil, nil
	}
	pcm, err := decode(payload)
	if err != nil {
		return nil, err
	}
	if len(pcm) == 0 {
		return nil, nil
	}
	return &PCMFrame{TcpPacket: TcpPacket{Packet: packet, SequenceNumber: uint32(packet.SequenceNumber)}, pcmData: pcm}, nil
}

// receiveClassicControl handles sync packets, which anchor RTP time to the
// sender's NTP clock, and resent audio packets.
func (s *Server) receiveClassicControl(conn *net.UDPConn, link *realtimeControlLink, done <-chan struct{}, finish func()) {
	defer finish()
	buffer := make([]byte, 4096)
	announced := false
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		s.touch()
		if n < 4 {
			continue
		}
		switch buffer[1] & 0x7f {
		case 0x54: // sync
			if n < 20 {
				continue
			}
			stamp, networkTime, latency := parseClassicSync(buffer[:n])
			s.anchor(networkTime, stamp)
			if !announced {
				s.noteLatency(time.Duration(int64(latency)+ClassicLatencyAllowance) * time.Second / time.Duration(sampleRate))
				log.Printf("AUDIO AirPlay 1 sync received; sender latency=%d samples (%d ms) plus %d", latency, int64(latency)*1000/sampleRate, ClassicLatencyAllowance)
				announced = true
			}
		case 0x56: // resent audio: a 4-byte header, then the original packet
			if n < 4+12 {
				continue
			}
			datagram := realtimeDatagram{data: append([]byte(nil), buffer[4:n]...), epoch: s.flushEpoch.Load()}
			select {
			case link.resent <- datagram:
			case <-done:
				return
			default:
			}
		}
	}
}

// parseClassicSync reads a sync packet: at the sender's NTP time (bytes
// 8..16) the sender is playing the frame at bytes 4..8, and the frame at
// 16..20 is the one it is sending, so their difference is the sender's
// latency. The receiver plays a frame ClassicLatencyAllowance samples after
// the sender (shairport-sync's Audio-Latency), so the returned stamp is the
// frame to play at networkTime.
func parseClassicSync(packet []byte) (stamp uint32, networkTime int64, latency int32) {
	playing := binary.BigEndian.Uint32(packet[4:8])
	networkTime = ntp.FromNTP(packet[8:16])
	latency = int32(binary.BigEndian.Uint32(packet[16:20]) - playing)
	return playing - ClassicLatencyAllowance, networkTime, latency
}
