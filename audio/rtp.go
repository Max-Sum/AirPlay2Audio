package audio

import (
	"encoding/binary"
	"fmt"
	"github.com/brutella/hc/crypto/chacha20poly1305"
	"github.com/pion/rtp"
)

type TcpPacket struct {
	rtp.Packet
	SequenceNumber uint32
}

type PCMFrame struct {
	TcpPacket
	pcmData []byte
	aac     bool // decoded from AAC: muted after a discontinuity
}

// muteAAC silences an AAC frame. Its first block after a seek, flush or gap
// carries decoder start-up noise (shairport-sync mutes it the same way).
func (p *PCMFrame) muteAAC() {
	if p.aac {
		clear(p.pcmData)
	}
}

func (p *PCMFrame) Data() []byte {
	return p.pcmData
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
