package audio

import (
	"encoding/binary"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	alac "github.com/mycophonic/saprobe-alac"
	"github.com/Max-Sum/AirPlay2Audio/codec"
)

// In AirPlay 2 buffered (type 103) audio, the RTP SSRC of every packet names
// its encoding, and a sender may change it during a stream. The values are
// those shairport-sync recognizes (player.h).
const (
	ssrcALAC48000S24 = 0x15000000
	ssrcAAC44100     = 0x16000000
	ssrcAAC48000     = 0x17000000
	ssrcAAC48000x51  = 0x27000000
	ssrcAAC48000x71  = 0x28000000
)

// bufferedFormat is the encoding of buffered audio packets.
type bufferedFormat struct {
	name     string
	rate     int
	alac     bool
	bits     uint8 // ALAC sample size
	channels int
}

var (
	formatAAC44100     = bufferedFormat{name: "AAC/44100/2", rate: 44100, channels: 2}
	formatAAC48000     = bufferedFormat{name: "AAC/48000/2", rate: 48000, channels: 2}
	formatALAC48000S24 = bufferedFormat{name: "ALAC/48000/S24/2", rate: 48000, alac: true, bits: 24, channels: 2}
)

// codecName is the codec shown by the monitor.
func (f bufferedFormat) codecName() string {
	if f.alac {
		return "alac"
	}
	return "aac"
}

var (
	formatAAC48000x51 = bufferedFormat{name: "AAC/48000/5.1", rate: 48000, channels: 6}
	formatAAC48000x71 = bufferedFormat{name: "AAC/48000/7.1", rate: 48000, channels: 8}
)

var bufferedFormats = map[uint32]bufferedFormat{
	ssrcAAC44100:     formatAAC44100,
	ssrcAAC48000:     formatAAC48000,
	ssrcALAC48000S24: formatALAC48000S24,
	// Multichannel AAC is downmixed to stereo by the decoder.
	ssrcAAC48000x51: formatAAC48000x51,
	ssrcAAC48000x71: formatAAC48000x71,
}

// SETUP audioFormat bits (also used in /info supportedFormats).
const (
	audioFormatALAC44100S16 = 1 << 18
	audioFormatALAC48000S24 = 1 << 21
	audioFormatAAC44100     = 1 << 22
	audioFormatAAC48000     = 1 << 23
	audioFormatAAC48000x51  = 1 << 39
	audioFormatAAC48000x71  = 1 << 40
)

// RealtimeFormats and BufferedFormats are the formats advertised in /info,
// as shairport-sync advertises them: realtime ALAC 44.1 kHz; buffered AAC at
// 44.1 and 48 kHz, lossless ALAC 48 kHz 24-bit, and AAC 5.1 and 7.1.
const (
	RealtimeFormats uint64 = audioFormatALAC44100S16
	BufferedFormats uint64 = audioFormatAAC44100 | audioFormatAAC48000 | audioFormatALAC48000S24 |
		audioFormatAAC48000x51 | audioFormatAAC48000x71
)

// formatForAudioFormat returns the encoding named by a SETUP audioFormat
// bit. It is used for packets whose SSRC names no known encoding, and to
// interpret anchors that arrive before the first packet.
func formatForAudioFormat(audioFormat uint64) bufferedFormat {
	switch audioFormat {
	case audioFormatAAC48000:
		return formatAAC48000
	case audioFormatALAC48000S24:
		return formatALAC48000S24
	case audioFormatAAC48000x51:
		return formatAAC48000x51
	case audioFormatAAC48000x71:
		return formatAAC48000x71
	}
	return formatAAC44100
}

// SupportedBufferedFormat reports whether a SETUP audioFormat can be played.
func SupportedBufferedFormat(audioFormat uint64) bool {
	return audioFormat != 0 && audioFormat&BufferedFormats == audioFormat && audioFormat&(audioFormat-1) == 0
}

// aacConfig is the AudioSpecificConfig of an AAC-LC format.
func aacConfig(f bufferedFormat) []byte {
	switch {
	case f.channels == 6:
		return []byte{0x11, 0xb0} // 48000 Hz, channel configuration 6 (5.1)
	case f.channels == 8:
		return []byte{0x11, 0xb8} // 48000 Hz, channel configuration 7 (7.1)
	case f.rate == 48000:
		return []byte{0x11, 0x90}
	}
	return []byte{0x12, 0x10} // 44100 Hz stereo
}

// rtpMapper converts a stream's RTP timestamps to the player's 44.1 kHz
// timeline. At 44.1 kHz it changes nothing. At another rate it extends the
// 32-bit timestamps to 64 bits, so the converted values stay continuous
// across the 32-bit wrap, and scales them. Anchors and packets of one stream
// go through the same mapper, so they stay consistent.
type rtpMapper struct {
	mu      sync.Mutex
	rate    int
	have    bool
	lastTS  uint32
	lastExt int64
}

func newRTPMapper(rate int) *rtpMapper { return &rtpMapper{rate: rate} }

// setRate changes the source rate and reports whether it changed; the
// mapping then starts again from the next timestamp.
func (m *rtpMapper) setRate(rate int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rate == m.rate {
		return false
	}
	m.rate, m.have = rate, false
	return true
}

func (m *rtpMapper) Map(ts uint32) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rate == 0 || m.rate == int(sampleRate) {
		return ts
	}
	if !m.have {
		m.have, m.lastTS, m.lastExt = true, ts, int64(ts)
	}
	ext := m.lastExt + int64(int32(ts-m.lastTS))
	m.lastTS, m.lastExt = ts, ext
	scaled := ext * sampleRate
	quotient := scaled / int64(m.rate)
	if scaled%int64(m.rate) != 0 && scaled < 0 {
		quotient-- // floor, so negative values map continuously too
	}
	return uint32(quotient)
}

// bufferedDecoder decodes buffered packets according to their SSRC and
// converts them to the player's 44.1 kHz 16-bit stereo.
type bufferedDecoder struct {
	fallback bufferedFormat
	mapper   *rtpMapper
	// onRateChange is called after the source rate changed, before the
	// packet is mapped, so the stream can map its anchor again.
	onRateChange func()
	// onFormat is called when the encoding is first seen or changes; onBytes
	// with the size of each compressed payload. Both are optional.
	onFormat func(bufferedFormat)
	onBytes  func(int)

	format    bufferedFormat
	ready     bool
	aac       *codec.AacDecoder
	alac      *alac.PacketDecoder
	resampler *codec.StreamResampler
	next      uint32 // player timestamp following the last resampled frame
	haveNext  bool
	reported  map[uint32]bool

	// AAC muting, as shairport-sync: the first block of each play sequence
	// and the first block after a timestamp gap are silenced.
	restart     *atomic.Uint64 // the stream's restart counter; nil in tests
	seenRestart uint64
	expectTS    uint32 // source timestamp of the next contiguous block
	haveExpect  bool
}

// mute reports whether a decoded AAC block must be silenced, and records
// where the next block should start.
func (d *bufferedDecoder) mute(timestamp uint32, samples int) bool {
	if d.restart != nil {
		if n := d.restart.Load(); n != d.seenRestart {
			d.seenRestart, d.haveExpect = n, false
		}
	}
	gap := !d.haveExpect || timestamp != d.expectTS
	d.expectTS, d.haveExpect = timestamp+uint32(samples), true
	return gap
}

func newBufferedDecoder(fallback bufferedFormat, mapper *rtpMapper) *bufferedDecoder {
	return &bufferedDecoder{fallback: fallback, mapper: mapper, reported: map[uint32]bool{}}
}

func (d *bufferedDecoder) Close() {
	if d.aac != nil {
		d.aac.Close()
		d.aac = nil
	}
}

// formatFor returns the encoding of a packet with this SSRC.
func (d *bufferedDecoder) formatFor(ssrc uint32) bufferedFormat {
	if f, ok := bufferedFormats[ssrc]; ok {
		return f
	}
	if !d.reported[ssrc] && len(d.reported) < 16 {
		d.reported[ssrc] = true
		log.Printf("AUDIO buffered SSRC %#08x names no known encoding; decoding as %s from SETUP", ssrc, d.fallback.name)
	}
	return d.fallback
}

func (d *bufferedDecoder) switchTo(f bufferedFormat) error {
	d.Close()
	d.alac, d.resampler, d.haveNext, d.haveExpect = nil, nil, false, false
	if f.alac {
		decoder, err := alac.NewPacketDecoder(alac.PacketConfig{
			FrameLength: 352, BitDepth: f.bits, NumChannels: 2,
			PB: 40, MB: 10, KB: 14, MaxRun: 255, SampleRate: uint32(f.rate),
		})
		if err != nil {
			return fmt.Errorf("initialize ALAC decoder: %w", err)
		}
		d.alac = decoder
	} else {
		d.aac = codec.NewAacDecoder()
		if err := d.aac.InitRaw(aacConfig(f)); err != nil {
			d.aac = nil
			return fmt.Errorf("initialize AAC decoder: %w", err)
		}
		if f.channels != 2 {
			// The output is stereo: let the decoder downmix 5.1 and 7.1.
			if err := d.aac.SetStereoOutput(); err != nil {
				d.Close()
				return fmt.Errorf("enable AAC downmix: %w", err)
			}
		}
	}
	if d.ready {
		log.Printf("AUDIO buffered encoding changed from %s to %s", d.format.name, f.name)
	} else {
		log.Printf("AUDIO buffered encoding %s", f.name)
	}
	d.format, d.ready = f, true
	if d.onFormat != nil {
		d.onFormat(f)
	}
	if d.mapper != nil && d.mapper.setRate(f.rate) && d.onRateChange != nil {
		d.onRateChange()
	}
	return nil
}

// decode returns the PCM frame for a decrypted payload, or nil when the
// packet holds no audio that can be played.
func (d *bufferedDecoder) decode(packet TcpPacket, payload []byte) (*PCMFrame, error) {
	f := d.formatFor(packet.SSRC)
	if f.channels != 2 && f.alac {
		if !d.reported[packet.SSRC] {
			d.reported[packet.SSRC] = true
			log.Printf("AUDIO buffered encoding %s is not supported; its packets are skipped", f.name)
		}
		return nil, nil
	}
	if !d.ready || f != d.format {
		if err := d.switchTo(f); err != nil {
			return nil, err
		}
	}
	if d.onBytes != nil {
		d.onBytes(len(payload))
	}
	var pcm []byte
	var err error
	if f.alac {
		pcm, err = d.alac.DecodePacket(payload)
	} else {
		pcm, err = d.aac.Decode(payload)
	}
	if err != nil || len(pcm) == 0 {
		return nil, err
	}
	aac := !f.alac
	if aac && d.mute(packet.Timestamp, len(pcm)/4) {
		clear(pcm)
	}
	if f.rate == int(sampleRate) && (!f.alac || f.bits == 16) {
		// Already the player's format and timeline: the common path.
		return &PCMFrame{TcpPacket: packet, pcmData: pcm, aac: aac}, nil
	}
	samples := toStereo16(pcm, f)
	if f.rate == int(sampleRate) {
		return &PCMFrame{TcpPacket: packet, pcmData: int16Bytes(samples), aac: aac}, nil
	}
	frame := d.resample(packet, samples)
	if frame != nil {
		frame.aac = aac
	}
	return frame, nil
}

// resample converts a block to 44.1 kHz. Output timestamps follow on from
// the previous block, so consecutive blocks join exactly; a jump in the
// source timestamps (seek, gap) restarts the resampler at the new position.
func (d *bufferedDecoder) resample(packet TcpPacket, samples []int16) *PCMFrame {
	mapped := d.mapper.Map(packet.Timestamp)
	if !d.haveNext || abs32(int32(mapped-d.next)) > 64 {
		d.resampler = codec.NewStreamResampler(d.format.rate, int(sampleRate))
		d.next, d.haveNext = mapped, true
	}
	out := d.resampler.Process(samples)
	packet.Timestamp = d.next
	d.next += uint32(len(out) / 2)
	if len(out) == 0 {
		return nil
	}
	return &PCMFrame{TcpPacket: packet, pcmData: int16Bytes(out)}
}

// toStereo16 reads decoded interleaved stereo as int16, rounding 24-bit
// samples.
func toStereo16(pcm []byte, f bufferedFormat) []int16 {
	if !f.alac || f.bits == 16 {
		out := make([]int16, len(pcm)/2)
		for i := range out {
			out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		}
		return out
	}
	out := make([]int16, len(pcm)/3)
	for i := range out {
		b := pcm[i*3:]
		v := int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
		v = (v + 128) >> 8
		if v > 32767 {
			v = 32767
		}
		out[i] = int16(v)
	}
	return out
}

func int16Bytes(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}
