package audio

import (
	"fmt"
	"log"
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
	audioFormatALAC44100S24 = 1 << 19
	audioFormatALAC48000S16 = 1 << 20
	audioFormatALAC48000S24 = 1 << 21
	audioFormatAAC44100     = 1 << 22
	audioFormatAAC48000     = 1 << 23
	audioFormatAAC48000x51  = 1 << 39
	audioFormatAAC48000x71  = 1 << 40
)

// RealtimeFormats and BufferedFormats are the formats advertised in /info:
// realtime ALAC stereo at 44.1 or 48 kHz, 16 or 24-bit (shairport-sync
// advertises only 44.1 kHz 16-bit here); buffered AAC at 44.1 and 48 kHz,
// lossless ALAC 48 kHz 24-bit, and AAC 5.1 and, where the AAC decoder
// supports it, 7.1.
const (
	RealtimeFormats uint64 = audioFormatALAC44100S16 | audioFormatALAC44100S24 |
		audioFormatALAC48000S16 | audioFormatALAC48000S24
	BufferedFormats uint64 = audioFormatAAC44100 | audioFormatAAC48000 | audioFormatALAC48000S24 |
		audioFormatAAC48000x51 | audioFormat71
)

// formatForAudioFormat returns the encoding named by a SETUP audioFormat
// bit. It is used for packets whose SSRC names no known encoding.
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

// pcmFormat is the format of the decoded audio: AAC decodes to 16 bits.
func (f bufferedFormat) pcmFormat() codec.Format {
	bits := 16
	if f.alac {
		bits = int(f.bits)
	}
	return codec.Format{Rate: f.rate, Bits: bits}
}

// bufferedDecoder decodes buffered packets according to their SSRC. Frames
// keep the stream's own rate and sample size.
type bufferedDecoder struct {
	fallback bufferedFormat
	// onFormat is called when the encoding is first seen or changes; onBytes
	// with the size of each compressed payload. Both are optional.
	onFormat func(bufferedFormat)
	onBytes  func(int)

	format   bufferedFormat
	ready    bool
	aac      *codec.AacDecoder
	alac     *alac.PacketDecoder
	reported map[uint32]bool

	// pending holds the packets whose AAC audio the decoder has not output
	// yet, oldest first. The Windows decoder outputs each access unit one
	// unit late, so decoded audio takes the timestamp of the packet it came
	// from rather than that of the packet just given to the decoder.
	pending []TcpPacket

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

// maxPendingAAC bounds pending if the decoder holds back more audio than
// expected.
const maxPendingAAC = 4

func newBufferedDecoder(fallback bufferedFormat) *bufferedDecoder {
	return &bufferedDecoder{fallback: fallback, reported: map[uint32]bool{}}
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
	d.alac, d.haveExpect, d.pending = nil, false, nil
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
	return nil
}

// decode returns the PCM frame for a decrypted payload, or nil when the
// packet holds no audio that can be played.
func (d *bufferedDecoder) decode(packet TcpPacket, payload []byte) (*PCMFrame, error) {
	f := d.formatFor(packet.SSRC)
	if f.channels != 2 && f.alac || f.channels == 8 && !decodes71 {
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
		d.pending = append(d.pending, packet)
		pcm, err = d.aac.Decode(payload)
		if err != nil {
			// The decoder was flushed: audio it held back is gone.
			d.pending = nil
		} else if len(pcm) > 0 {
			packet = d.pending[0]
			d.pending = d.pending[1:]
		} else if len(d.pending) > maxPendingAAC {
			d.pending = d.pending[1:]
		}
	}
	if err != nil || len(pcm) == 0 {
		return nil, err
	}
	format := f.pcmFormat()
	var samples []int32
	if format.Bits == 24 {
		samples = pcm24(pcm)
	} else {
		samples = pcm16(pcm)
	}
	aac := !f.alac
	if aac && d.mute(packet.Timestamp, len(samples)/2) {
		clear(samples)
	}
	return &PCMFrame{TcpPacket: packet, samples: samples, format: format, aac: aac}, nil
}
