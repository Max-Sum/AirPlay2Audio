package handlers

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Max-Sum/AirPlay2Audio/audio"
	"github.com/Max-Sum/AirPlay2Audio/ptp"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
)

func request(method, path string, header rtsp.Header, body string) *rtsp.Request {
	if header == nil {
		header = rtsp.Header{}
	}
	return &rtsp.Request{Method: rtsp.Method(method), Path: path, Header: header, Body: []byte(body)}
}

func TestOptionsStarIsParsed(t *testing.T) {
	var req rtsp.Request
	raw := "OPTIONS * RTSP/1.0\r\nCSeq: 1\r\nApple-Challenge: abc\r\n\r\n"
	if err := req.Read(bufio.NewReader(strings.NewReader(raw))); err != nil {
		t.Fatal(err)
	}
	if req.Method != "OPTIONS" || req.Path != "*" || req.Header["Apple-Challenge"][0] != "abc" {
		t.Fatalf("parsed %+v", req)
	}
}

func TestParseSDP(t *testing.T) {
	key, _ := loadAirportKey()
	aesKey := []byte("0123456789abcdef")
	encrypted, _ := rsa.EncryptOAEP(sha1.New(), rand.Reader, &key.PublicKey, aesKey, nil)
	sdp := "v=0\r\no=iTunes 3413821438 0 IN IP4 192.168.1.2\r\ns=iTunes\r\nc=IN IP4 192.168.1.20\r\nt=0 0\r\n" +
		"m=audio 0 RTP/AVP 96\r\na=rtpmap:96 AppleLossless\r\na=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\r\n" +
		"a=rsaaeskey:" + base64.RawStdEncoding.EncodeToString(encrypted) + "\r\na=aesiv:" + base64.RawStdEncoding.EncodeToString([]byte("fedcba9876543210")) + "\r\n"
	format, err := parseSDP([]byte(sdp))
	if err != nil {
		t.Fatal(err)
	}
	if format.Codec != "alac" || string(format.Key) != string(aesKey) || format.ALAC.FrameLength != 352 || format.ALAC.SampleRate != 44100 || format.ALAC.NumChannels != 2 || format.ALAC.KB != 14 {
		t.Fatalf("format %+v", format)
	}
	if _, err := parseSDP([]byte("m=audio 0 RTP/AVP 96\r\na=rtpmap:96 mpeg4-generic/44100/2\r\n")); err == nil {
		t.Fatal("AAC accepted")
	}
	if _, err := parseSDP([]byte("a=rtpmap:96 AppleLossless\r\na=fpaeskey:AAAA\r\n")); err == nil {
		t.Fatal("FairPlay accepted")
	}
	format, err = parseSDP([]byte("a=rtpmap:96 L16/44100/2\r\n"))
	if err != nil || format.Codec != "l16" || format.Key != nil {
		t.Fatalf("L16: %+v %v", format, err)
	}
}

func TestTransportPorts(t *testing.T) {
	control, timing := transportPorts("RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;control_port=6001;timing_port=6002")
	if control != 6001 || timing != 6002 {
		t.Fatalf("got %d %d", control, timing)
	}
}

func TestClassicSessionHandshake(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, sender := tcpPair(t, listener)
	defer sender.Close()
	player := audio.NewPlayer(ptp.NewVirtualClock(0), audio.NewRing(16))
	var sources []net.IP
	sourcesSet := false
	r := &Rstp{
		streams: map[string]*audio.Server{}, owners: map[string]*rtsp.Conn{}, sessions: map[*rtsp.Conn]bool{},
		auth: newPasswordAuth(""), player: player,
		HardwareAddr: net.HardwareAddr{1, 2, 3, 4, 5, 6},
		ClockSources: func(ips []net.IP) { sources, sourcesSet = ips, true },
	}
	resp, _ := r.Handle(conn, request("OPTIONS", "*", rtsp.Header{"Apple-Challenge": {"AAECAwQFBgcICQoLDA0ODw"}}, ""))
	if resp.StatusCode != 200 || len(resp.Header["Apple-Response"]) != 1 || !strings.Contains(resp.Header["Public"][0], "ANNOUNCE") {
		t.Fatalf("OPTIONS: %+v", resp)
	}
	sdp := "a=rtpmap:96 AppleLossless\r\na=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\r\n"
	resp, _ = r.Handle(conn, request("ANNOUNCE", "3413821438", rtsp.Header{"Content-Type": {"application/sdp"}}, sdp))
	if resp.StatusCode != 200 || !r.sessions[conn] || r.active != conn {
		t.Fatalf("ANNOUNCE: %+v", resp)
	}
	resp, _ = r.Handle(conn, request("SETUP", "3413821438", rtsp.Header{"Transport": {"RTP/AVP/UDP;unicast;mode=record;control_port=6001;timing_port=6002"}}, ""))
	if resp.StatusCode != 200 || !strings.Contains(resp.Header["Transport"][0], "server_port=") || !strings.Contains(resp.Header["Transport"][0], "timing_port=") {
		t.Fatalf("SETUP: %+v", resp)
	}
	if !sourcesSet || len(sources) != 0 {
		t.Fatalf("PTP still followed during AirPlay 1: %v", sources)
	}
	resp, _ = r.Handle(conn, request("RECORD", "3413821438", rtsp.Header{"Rtp-Info": {"seq=1;rtptime=0"}}, ""))
	if resp.StatusCode != 200 || resp.Header["Audio-Latency"][0] != "11025" {
		t.Fatalf("RECORD: %+v", resp)
	}
	resp, _ = r.Handle(conn, request("SET_PARAMETER", "3413821438", rtsp.Header{"Content-Type": {"text/parameters"}}, "volume: -15.000000\r\n"))
	if resp.StatusCode != 200 {
		t.Fatalf("SET_PARAMETER: %+v", resp)
	}
	resp, _ = r.Handle(conn, request("FLUSH", "3413821438", nil, ""))
	if resp.StatusCode != 200 {
		t.Fatalf("FLUSH: %+v", resp)
	}
	resp, _ = r.Handle(conn, request("TEARDOWN", "3413821438", nil, ""))
	if resp.StatusCode != 200 || len(r.streams) != 0 {
		t.Fatalf("TEARDOWN: %+v streams=%d", resp, len(r.streams))
	}
}

func TestRefuseInterruption(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	first, firstSender := tcpPair(t, listener)
	second, secondSender := tcpPair(t, listener)
	defer firstSender.Close()
	defer secondSender.Close()
	player := audio.NewPlayer(ptp.NewVirtualClock(0), audio.NewRing(16))
	r := &Rstp{
		streams: map[string]*audio.Server{}, owners: map[string]*rtsp.Conn{},
		sessions: map[*rtsp.Conn]bool{first: true}, active: first,
		auth: newPasswordAuth(""), player: player, RefuseInterruption: true,
	}
	sdp := "a=rtpmap:96 L16/44100/2\r\n"
	resp, _ := r.Handle(second, request("ANNOUNCE", "1", rtsp.Header{"Content-Type": {"application/sdp"}}, sdp))
	if resp.StatusCode != rtsp.StatusNotEnoughBandwidth || r.active != first {
		t.Fatalf("second sender not refused: %d", resp.StatusCode)
	}
	r.RefuseInterruption = false
	resp, _ = r.Handle(second, request("ANNOUNCE", "1", rtsp.Header{"Content-Type": {"application/sdp"}}, sdp))
	if resp.StatusCode != 200 || r.active != second {
		t.Fatalf("takeover failed: %d", resp.StatusCode)
	}
}

func TestExpireIdleClosesSilentSession(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, sender := tcpPair(t, listener)
	defer sender.Close()
	r := &Rstp{sessions: map[*rtsp.Conn]bool{conn: true}, lastSeen: map[*rtsp.Conn]time.Time{conn: time.Now().Add(-time.Minute)}}
	r.ExpireIdle(time.Hour)
	sender.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := sender.Read(make([]byte, 1)); err == nil || !isTimeout(err) {
		t.Fatal("active session closed")
	}
	r.ExpireIdle(30 * time.Second)
	sender.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := sender.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatal("idle session left open")
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
