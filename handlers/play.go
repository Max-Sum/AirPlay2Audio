package handlers

import (
	"github.com/Max-Sum/AirPlay2Audio/audio"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
	"howett.net/plist"
	"log"
	"math"
	"math/big"
	"strings"
	"time"
)

type params struct {
	SupportedCommand [][]byte `plist:"mrSupportedCommandsFromSender"`
}

type command struct {
	Type   string `plist:"type"`
	Params params `plist:"params"`
}

type setRateAnchorTime struct {
	Rate          uint32 `plist:"rate"`
	RtpTime       uint32 `plist:"rtpTime"`
	Fraction      uint64 `plist:"networkTimeFrac"`
	Seconds       uint64 `plist:"networkTimeSecs"`
	ClockIdentity uint64 `plist:"networkTimeTimelineID"`
}

func (s *setRateAnchorTime) StartTime() time.Time {
	mantissa := big.NewFloat(float64(s.Fraction))
	result := big.NewFloat(0).SetMantExp(mantissa, -64)
	result.Mul(result, big.NewFloat(math.Pow(10, 9)))
	value, _ := result.Int64()
	return time.Unix(int64(s.Seconds), value)
}

func (r *Rstp) OnRecordWeb(req *rtsp.Request) (*rtsp.Response, error) {
	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		var content map[string]interface{}
		if _, err := plist.Unmarshal(req.Body, &content); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
		}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK, Header: rtsp.Header{"Audio-Latency": rtsp.HeaderValue{"0"}}}, nil
}

func (r *Rstp) OnAudioMode(req *rtsp.Request) (*rtsp.Response, error) {
	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		var content map[string]interface{}
		if _, err := plist.Unmarshal(req.Body, &content); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
		}
		// These field names are protocol metadata. Do not log values, which
		// may contain information about the sender or media being played.
		for _, name := range []string{"audioMode", "volume", "volumeScalar", "volumeControlType"} {
			if value, ok := content[name]; ok {
				log.Printf("CONTROL audioMode field %s (%T)", name, value)
			}
		}
		log.Printf("CONTROL audioMode fields=%d", len(content))
	}

	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

func (r *Rstp) OnSetRateAnchorTime(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		var content setRateAnchorTime
		if _, err := plist.Unmarshal(req.Body, &content); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
		}
		if s, found := r.streams[req.Path]; found {
			if content.Rate == 0 {
				s.SetRate0()
			} else {
				// The sender that starts playing last takes the output.
				r.startPlaying(conn)
				if r.AnchorClock != nil && content.ClockIdentity != 0 {
					r.AnchorClock(content.ClockIdentity)
				}
				s.SetRateAnchorTime(content.RtpTime, content.StartTime())
			}
			if r.foreground(conn) {
				setPlaybackState(content.Rate != 0)
			}
		}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

func (r *Rstp) OnCommand(req *rtsp.Request) (*rtsp.Response, error) {
	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		var content command
		if _, err := plist.Unmarshal(req.Body, &content); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
		}
		// Command type is protocol metadata, not the media metadata contained
		// in params. Restrict its alphabet and length before logging.
		if isProtocolIdentifier(content.Type) {
			log.Printf("CONTROL command %s", content.Type)
		}
		diagnoseCommandFields(req.Body)
		diagnoseSupportedCommands(content.Params.SupportedCommand)
		for _, value := range content.Params.SupportedCommand {
			var commandContent map[string]interface{}
			if _, err := plist.Unmarshal(value, &commandContent); err != nil {
				return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
			}
		}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

func isProtocolIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func (r *Rstp) OnTeardownWeb(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	var content map[string]interface{}
	if _, err := plist.Unmarshal(req.Body, &content); err != nil {
		return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
	}
	if s, found := r.streams[req.Path]; found {
		s.Teardown()
		// Realtime senders tear the audio stream down a few seconds after
		// a pause and set it up again on resume.
		if r.foreground(conn) {
			setPlaybackState(false)
		}
		// if streams is present we pause otherwise we drop the stream
		if _, stream := content["streams"]; !stream {
			delete(r.streams, req.Path)
			delete(r.owners, req.Path)
		}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

// OnFlushBuffered handles FLUSHBUFFERED as shairport-sync does. Without
// flushFromSeq it is an immediate flush: everything queued and everything
// that arrives before flushUntilSeq is dropped, and playback stops until the
// next SETRATEANCHORTIME (seek, track change). With flushFromSeq it is a
// deferred flush of the packets from flushFromSeq up to flushUntilSeq,
// wherever they are, while playback goes on (the sender replaces the queued
// next track).
func (r *Rstp) OnFlushBuffered(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		var content map[string]interface{}
		if _, err := plist.Unmarshal(req.Body, &content); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
		}
		if s, found := r.streams[req.Path]; found {
			until, ok := content["flushUntilSeq"].(uint64)
			if !ok {
				return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
			}
			value, deferred := content["flushFromSeq"]
			if !deferred {
				log.Printf("AUDIO immediate flush until %d", until&audio.SeqMask)
				s.FlushImmediate(until)
				return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
			}
			from, ok := value.(uint64)
			if !ok {
				return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
			}
			log.Printf("AUDIO deferred flush %d..%d", from&audio.SeqMask, until&audio.SeqMask)
			s.Flush(from, until)
		}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

// OnSetRate handles SETRATE, a rate change without a new anchor.
// shairport-sync answers it with 501; here rate 0 pauses like
// SETRATEANCHORTIME with rate 0, and another rate is acknowledged: playback
// starts again with the anchor of the next SETRATEANCHORTIME.
func (r *Rstp) OnSetRate(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	var content map[string]interface{}
	if _, err := plist.Unmarshal(req.Body, &content); err != nil {
		return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
	}
	rate, ok := plistNumber(content["rate"])
	if !ok {
		return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
	}
	log.Printf("CONTROL SETRATE rate=%g", rate)
	if s, found := r.streams[req.Path]; found && rate == 0 {
		s.SetRate0()
		if r.foreground(conn) {
			setPlaybackState(false)
		}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

// plistNumber reads an integer or real plist value.
func plistNumber(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case uint64:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	}
	return 0, false
}

// Realtime streams use FLUSH rather than FLUSHBUFFERED when starting or seeking.
func (r *Rstp) OnFlushRealtime(req *rtsp.Request) (*rtsp.Response, error) {
	if s, found := r.streams[req.Path]; found {
		s.ResetBuffer()
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}
