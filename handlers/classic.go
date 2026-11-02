package handlers

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	"goplay2/audio"
	"goplay2/rtsp"

	alac "github.com/mycophonic/saprobe-alac"
)

// AirPlay 1 (RAOP) sessions: OPTIONS with an Apple-Challenge, ANNOUNCE
// carrying an SDP description of the stream, SETUP with an RTP Transport
// header, RECORD, then SET_PARAMETER, FLUSH and TEARDOWN. Audio and timing
// use UDP; see audio.SetupClassic.

type classicSession struct {
	path   string
	format audio.ClassicFormat
	stream *audio.Server
}

const publicMethods = "ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, FLUSHBUFFERED, TEARDOWN, OPTIONS, GET_PARAMETER, SET_PARAMETER, POST, GET, SETPEERS, SETPEERSX, SETRATEANCHORTIME, SETRATE"

func (r *Rstp) OnOptions(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	header := rtsp.Header{"Public": rtsp.HeaderValue{publicMethods}}
	if challenge := headerValue(req, "Apple-Challenge"); challenge != "" {
		local := localIP(conn)
		response, err := appleResponse(challenge, local, r.HardwareAddr)
		if err != nil {
			log.Printf("RAOP cannot answer Apple-Challenge: %v", err)
			return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
		}
		header["Apple-Response"] = rtsp.HeaderValue{response}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK, Header: header}, nil
}

func localIP(conn *rtsp.Conn) net.IP {
	if addr, ok := conn.NetConn().LocalAddr().(*net.TCPAddr); ok {
		return addr.IP
	}
	return net.IPv4zero
}

// parseSDP reads the stream description of an ANNOUNCE.
func parseSDP(body []byte) (audio.ClassicFormat, error) {
	var format audio.ClassicFormat
	var rtpmap, fmtp, rsaKey, iv string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		name, value, found := strings.Cut(line, ":")
		if !found || !strings.HasPrefix(name, "a=") {
			continue
		}
		switch name[2:] {
		case "rtpmap":
			rtpmap = value
		case "fmtp":
			fmtp = value
		case "rsaaeskey":
			rsaKey = value
		case "aesiv":
			iv = value
		case "fpaeskey":
			return format, fmt.Errorf("FairPlay-encrypted AirPlay 1 streams are not supported")
		}
	}
	fields := strings.Fields(rtpmap)
	if len(fields) < 2 {
		return format, fmt.Errorf("no rtpmap in stream description")
	}
	encoding := strings.ToLower(fields[1])
	switch {
	case strings.HasPrefix(encoding, "applelossless"):
		format.Codec = "alac"
		values := strings.Fields(fmtp)
		if len(values) < 12 {
			return format, fmt.Errorf("ALAC fmtp has %d fields", len(values))
		}
		var n [11]uint64
		for i := range n {
			v, err := strconv.ParseUint(values[i+1], 10, 32)
			if err != nil {
				return format, fmt.Errorf("ALAC fmtp: %w", err)
			}
			n[i] = v
		}
		// frameLength compatibleVersion bitDepth pb mb kb numChannels
		// maxRun maxFrameBytes avgBitRate sampleRate
		format.ALAC = alac.PacketConfig{
			FrameLength: uint32(n[0]), BitDepth: uint8(n[2]), PB: uint8(n[3]), MB: uint8(n[4]), KB: uint8(n[5]),
			NumChannels: uint8(n[6]), MaxRun: uint16(n[7]), MaxFrameBytes: uint32(n[8]), AvgBitRate: uint32(n[9]), SampleRate: uint32(n[10]),
		}
		if format.ALAC.FrameLength == 0 || format.ALAC.FrameLength > 8192 {
			return format, fmt.Errorf("ALAC frame length %d", format.ALAC.FrameLength)
		}
	case strings.HasPrefix(encoding, "l16/44100/2"):
		format.Codec = "l16"
	default:
		return format, fmt.Errorf("unsupported AirPlay 1 encoding %q", fields[1])
	}
	if rsaKey != "" {
		key, err := decryptAESKey(rsaKey)
		if err != nil {
			return format, fmt.Errorf("decrypt AES key: %w", err)
		}
		vector, err := decodeBase64(iv)
		if err != nil || len(vector) != 16 {
			return format, fmt.Errorf("invalid AES IV")
		}
		format.Key, format.IV = key, vector
	}
	return format, nil
}

// interrupts reports whether conn would take over another sender's session
// while interruption is not allowed.
func (r *Rstp) interrupts(conn *rtsp.Conn) bool {
	return r.RefuseInterruption && r.active != nil && r.active != conn && r.sessions[r.active]
}

func busy() *rtsp.Response {
	log.Print("SESSION refused: another sender is playing")
	return &rtsp.Response{StatusCode: rtsp.StatusNotEnoughBandwidth}
}

func (r *Rstp) OnAnnounce(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	if !strings.EqualFold(headerValue(req, "Content-Type"), "application/sdp") {
		return &rtsp.Response{StatusCode: rtsp.StatusUnsupportedMediaType}, nil
	}
	if r.interrupts(conn) {
		return busy(), nil
	}
	format, err := parseSDP(req.Body)
	if err != nil {
		log.Printf("RAOP ANNOUNCE rejected: %v", err)
		return &rtsp.Response{StatusCode: rtsp.StatusUnsupportedMediaType}, nil
	}
	r.admit(conn)
	if old := r.classic[conn]; old != nil {
		r.dropStream(old.path)
	}
	if r.classic == nil {
		r.classic = make(map[*rtsp.Conn]*classicSession)
	}
	r.classic[conn] = &classicSession{path: "classic:" + req.Path, format: format}
	name := headerValue(req, "X-Apple-Client-Name")
	if name == "" {
		name = headerValue(req, "User-Agent")
	}
	if name == "" {
		name = "AirPlay 1"
	}
	log.Printf("RAOP AirPlay 1 stream: codec=%s encrypted=%t samples=%d", format.Codec, format.Key != nil, format.SamplesPerPacket())
	r.sessionStarted(conn, name)
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

// transportPorts reads control_port and timing_port from a Transport header.
func transportPorts(transport string) (control, timing int) {
	for _, part := range strings.Split(transport, ";") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		port, err := strconv.Atoi(value)
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		switch strings.ToLower(name) {
		case "control_port":
			control = port
		case "timing_port":
			timing = port
		}
	}
	return
}

func (r *Rstp) onClassicSetup(conn *rtsp.Conn, session *classicSession, req *rtsp.Request) (*rtsp.Response, error) {
	transport := headerValue(req, "Transport")
	if !strings.Contains(strings.ToUpper(transport), "UDP") {
		log.Printf("RAOP unsupported transport %q", transport)
		return &rtsp.Response{StatusCode: rtsp.StatusUnsupportedTransport}, nil
	}
	controlPort, timingPort := transportPorts(transport)
	remote, ok := conn.NetConn().RemoteAddr().(*net.TCPAddr)
	if !ok {
		return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
	}
	// SETUP is followed by RECORD at once: this sender starts playing.
	r.startPlaying(conn)
	stream, err := audio.NewServer(r.player)
	if err != nil {
		return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
	}
	if timingPort == 0 {
		log.Print("RAOP sender gave no timing port; playback may not be synchronized")
	}
	peer := audio.ClassicPeer{Addr: &net.UDPAddr{IP: remote.IP, Zone: remote.Zone}, ControlPort: controlPort, TimingPort: timingPort}
	dataPort, ourControl, ourTiming, err := stream.SetupClassic(session.format, peer)
	if err != nil {
		log.Printf("AUDIO AirPlay 1 setup error: %v", err)
		return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
	}
	if old, found := r.streams[session.path]; found {
		old.Teardown()
	}
	session.stream = stream
	r.streams[session.path] = stream
	r.owners[session.path] = conn
	// This sender's clock arrives over NTP; ignore PTP from anyone.
	if r.ClockSources != nil {
		r.ClockSources([]net.IP{})
	}
	setPlaybackState(true)
	return &rtsp.Response{StatusCode: rtsp.StatusOK, Header: rtsp.Header{
		"Transport":         rtsp.HeaderValue{fmt.Sprintf("RTP/AVP/UDP;unicast;mode=record;server_port=%d;control_port=%d;timing_port=%d", dataPort, ourControl, ourTiming)},
		"Session":           rtsp.HeaderValue{"1"},
		"Audio-Jack-Status": rtsp.HeaderValue{"connected; type=analog"},
	}}, nil
}

func (r *Rstp) onClassic(conn *rtsp.Conn, session *classicSession, req *rtsp.Request) (*rtsp.Response, error) {
	switch req.Method {
	case "SETUP":
		return r.onClassicSetup(conn, session, req)
	case "RECORD":
		r.startPlaying(conn)
		setPlaybackState(true)
		return &rtsp.Response{StatusCode: rtsp.StatusOK, Header: rtsp.Header{
			"Audio-Latency": rtsp.HeaderValue{strconv.Itoa(audio.ClassicLatencyAllowance)},
		}}, nil
	case "FLUSH":
		if session.stream != nil {
			session.stream.ResetBuffer()
		}
		return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
	case "TEARDOWN":
		r.dropStream(session.path)
		session.stream = nil
		if r.foreground(conn) {
			setPlaybackState(false)
		}
		return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
	}
	return nil, nil
}
