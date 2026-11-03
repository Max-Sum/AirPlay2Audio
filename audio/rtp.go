package audio

import (
	"encoding/binary"
	"fmt"
	"github.com/Max-Sum/AirPlay2Audio/codec"
	"github.com/brutella/hc/crypto/chacha20poly1305"
	"github.com/pion/rtp"
)

type TcpPacket struct {
	rtp.Packet
	SequenceNumber uint32
}

// PCMFrame is decoded stereo audio. Its timestamp counts frames at its own
// rate.
type PCMFrame struct {
	TcpPacket
	samples []int32 // interleaved stereo, left-aligned (see codec.FullScale)
	format  codec.Format
	aac     bool // decoded from AAC: muted after a discontinuity
}

// muteAAC silences an AAC frame. Its first block after a seek, flush or gap
// carries decoder start-up noise (shairport-sync mutes it the same way).
func (p *PCMFrame) muteAAC() {
	if p.aac {
		clear(p.samples)
	}
}

// Data returns the interleaved stereo samples.
func (p *PCMFrame) Data() []int32 { return p.samples }

// Format returns the rate and sample size of the frame.
func (p *PCMFrame) Format() codec.Format { return p.format }

// frames is the number of stereo frames.
func (p *PCMFrame) frames() int { return len(p.samples) / 2 }

// pcm16 reads little-endian 16-bit PCM.
func pcm16(pcm []byte) []int32 {
	out := make([]int32, len(pcm)/2)
	for i := range out {
		out[i] = int32(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) << 16
	}
	return out
}

// pcm24 reads packed little-endian 24-bit PCM.
func pcm24(pcm []byte) []int32 {
	out := make([]int32, len(pcm)/3)
	for i := range out {
		b := pcm[i*3:]
		out[i] = int32(uint32(b[0])<<8 | uint32(b[1])<<16 | uint32(b[2])<<24)
	}
	return out
}

// NewFrame authenticates and decrypts a buffered packet, then decodes it
// according to its SSRC.
func NewFrame(decoder *bufferedDecoder, sharedKey []byte, rawPacket []byte) (*PCMFrame, error) {
	var err error
	packet := TcpPacket{}
	if err = packet.Unmarshal(rawPacket); err != nil {
		return nil, err
	}
	if len(rawPacket) < 12 || len(packet.Payload) < 24 {
		return nil, fmt.Errorf("short buffered audio packet")
	}
	var seqBytes [4]byte
	copy(seqBytes[1:], rawPacket[1:4])
	packet.Marker = false  // used by apple in sequenceNumber
	packet.PayloadType = 0 // used by apple in sequenceNumber
	packet.SequenceNumber = binary.BigEndian.Uint32(seqBytes[:])
	message := packet.Payload[:len(packet.Payload)-24]
	nonce := packet.Payload[len(packet.Payload)-8:]
	var mac [16]byte
	copy(mac[:], packet.Payload[len(packet.Payload)-24:len(packet.Payload)-8])
	aad := packet.Raw[4:0xc]
	decrypted, err := chacha20poly1305.DecryptAndVerify(sharedKey, nonce, message, mac, aad)
	if err != nil {
		return nil, err
	}
	// Some senders emit an authenticated empty access unit when beginning or
	// changing streams. It is not an AAC decoder failure.
	if len(decrypted) == 0 {
		return nil, nil
	}
	return decoder.decode(packet, decrypted)
}
