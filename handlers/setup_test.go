package handlers

import (
	"net"
	"strings"
	"testing"

	"github.com/Max-Sum/AirPlay2Audio/audio"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/homekit"
	"github.com/Max-Sum/AirPlay2Audio/ptp"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
	"howett.net/plist"
)

func setupRequest(t *testing.T, path string, content interface{}) *rtsp.Request {
	t.Helper()
	body, err := plist.Marshal(content, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return &rtsp.Request{Method: "SETUP", Path: path, Body: body, Header: rtsp.Header{
		"Content-Type": rtsp.HeaderValue{"application/x-apple-binary-plist"},
	}}
}

func setupHandler(t *testing.T) (*Rstp, *rtsp.Conn) {
	t.Helper()
	server, err := homekit.NewServer("02:00:00:00:00:05", "Test", t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	previousServer, previousDevice := homekit.Server, homekit.Device
	t.Cleanup(func() { homekit.Server, homekit.Device = previousServer, previousDevice })
	homekit.Server = server
	homekit.Device = homekit.NewAccessory("02:00:00:00:00:05", "00000000-0000-0000-0000-000000000005", globals.NewFeatures())
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	conn, sender := tcpPair(t, listener)
	t.Cleanup(func() { sender.Close(); conn.Close() })
	player := audio.NewPlayer(ptp.NewVirtualClock(0), audio.NewRing(16))
	r := &Rstp{
		streams:  map[string]*audio.Server{},
		owners:   map[string]*rtsp.Conn{},
		sessions: map[*rtsp.Conn]bool{},
		auth:     newPasswordAuth(""),
		player:   player,
	}
	return r, conn
}

func TestPickStreamPrefersAudio(t *testing.T) {
	stream, ok := pickStream([]setupStreamRequest{{Type: streamRemoteControl}, {Type: streamBuffered}, {Type: streamRealtime}})
	if !ok || stream.Type != streamBuffered {
		t.Fatalf("picked %+v", stream)
	}
	if _, ok := pickStream([]setupStreamRequest{{Type: 42}}); ok {
		t.Fatal("unknown stream type was picked")
	}
}

// A remote-control-only sender gets the event port but no session, so it
// cannot take the output from the sender that is playing.
func TestRemoteControlOnlySetupIsNotASession(t *testing.T) {
	r, conn := setupHandler(t)
	response, err := r.OnSetupWeb(conn, setupRequest(t, "rc", map[string]interface{}{
		"isRemoteControlOnly": true, "timingProtocol": "None", "name": "Remote",
	}))
	if err != nil || response.StatusCode != rtsp.StatusOK {
		t.Fatalf("setup: %v %+v", err, response)
	}
	if len(r.sessions) != 0 || r.active != nil || len(r.streams) != 0 {
		t.Fatalf("remote control setup started a session: sessions=%d active=%v", len(r.sessions), r.active)
	}
}

func TestRemoteControlStreamIsAcknowledged(t *testing.T) {
	r, conn := setupHandler(t)
	response, err := r.OnSetupWeb(conn, setupRequest(t, "rc", map[string]interface{}{
		"streams": []interface{}{map[string]interface{}{"type": uint64(streamRemoteControl)}},
	}))
	if err != nil || response.StatusCode != rtsp.StatusOK {
		t.Fatalf("setup: %v %+v", err, response)
	}
	var content remoteControlResponse
	if _, err := plist.Unmarshal(response.Body, &content); err != nil || len(content.Streams) != 1 || content.Streams[0].Type != streamRemoteControl {
		t.Fatalf("response: %v %+v", err, content)
	}
}

// Realtime audio is decoded at 44.1 kHz only; another rate is refused rather
// than played at the wrong speed.
func TestRealtimeSetupRefusesOtherRates(t *testing.T) {
	r, conn := setupHandler(t)
	if response, _ := r.OnSetupWeb(conn, setupRequest(t, "s", map[string]interface{}{"name": "Phone", "timingProtocol": "PTP"})); response.StatusCode != rtsp.StatusOK {
		t.Fatalf("session setup: %+v", response)
	}
	response, _ := r.OnSetupWeb(conn, setupRequest(t, "s", map[string]interface{}{
		"streams": []interface{}{map[string]interface{}{
			"type": uint64(streamRealtime), "audioFormat": uint64(0x40000), "sr": uint64(48000), "spf": uint64(352), "shk": make([]byte, 32),
		}},
	}))
	if response.StatusCode != rtsp.StatusBadRequest {
		t.Fatalf("48 kHz realtime stream: status %d", response.StatusCode)
	}
}

// The group of the sender that owns the output is published in the TXT
// records (gid, gcgl, ReceiverSessionIsActive), and withdrawn when its
// session ends.
func TestSetupPublishesGroup(t *testing.T) {
	r, conn := setupHandler(t)
	published := 0
	r.advertise = func() { published++ }
	const group = "11111111-2222-3333-4444-555555555555"
	response, _ := r.OnSetupWeb(conn, setupRequest(t, "s", map[string]interface{}{
		"name": "Phone", "timingProtocol": "PTP", "groupUUID": group, "groupContainsGroupLeader": true,
	}))
	if response.StatusCode != rtsp.StatusOK {
		t.Fatalf("session setup: %+v", response)
	}
	r.publishGroup()
	records := strings.Join(homekit.Device.ToRecords(), " ")
	if !strings.Contains(records, "gid="+group) || !strings.Contains(records, "gcgl=1") || !strings.Contains(records, "flags=0x20004") {
		t.Fatalf("records after SETUP: %s", records)
	}
	if published != 1 {
		t.Fatalf("published %d times", published)
	}
	r.publishGroup()
	if published != 1 {
		t.Fatal("unchanged records published again")
	}
	r.OnConnClose(conn)
	records = strings.Join(homekit.Device.ToRecords(), " ")
	if strings.Contains(records, group) || !strings.Contains(records, "gcgl=0") || !strings.Contains(records, "flags=0x4") {
		t.Fatalf("records after the session: %s", records)
	}
}

func TestSetPeersXUsesAllAddresses(t *testing.T) {
	r, conn := setupHandler(t)
	body, _ := plist.Marshal([]interface{}{
		map[string]interface{}{"Addresses": []string{"192.168.1.20", "fe80::1"}, "ID": "a", "ClockID": uint64(7)},
		map[string]interface{}{"Addresses": []string{"192.168.1.21", "not-an-ip"}, "ID": "b"},
	}, plist.BinaryFormat)
	response, _ := r.OnSetPeersX(conn, &rtsp.Request{Body: body})
	if response.StatusCode != rtsp.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if peers := r.sender(conn).peers; strings.Join(peers, ",") != "192.168.1.20,fe80::1,192.168.1.21" {
		t.Fatalf("peers %v", peers)
	}
}

// Several streams in one SETUP: the audio stream is set up, the others are
// ignored instead of failing the request.
func TestBufferedSetupAcceptsSeveralStreams(t *testing.T) {
	r, conn := setupHandler(t)
	if response, _ := r.OnSetupWeb(conn, setupRequest(t, "s", map[string]interface{}{"name": "Phone", "timingProtocol": "PTP"})); response.StatusCode != rtsp.StatusOK {
		t.Fatalf("session setup: %+v", response)
	}
	defer r.streams["s"].Teardown()
	response, err := r.OnSetupWeb(conn, setupRequest(t, "s", map[string]interface{}{
		"streams": []interface{}{
			map[string]interface{}{"type": uint64(streamRemoteControl)},
			map[string]interface{}{"type": uint64(streamBuffered), "audioFormat": uint64(1 << 23), "spf": uint64(1024), "shk": make([]byte, 32)},
		},
	}))
	if err != nil || response.StatusCode != rtsp.StatusOK {
		t.Fatalf("setup: %v %+v", err, response)
	}
	var content setupSteamsResponse
	if _, err := plist.Unmarshal(response.Body, &content); err != nil || len(content.Streams) != 1 || content.Streams[0].TypeStream != streamBuffered || content.Streams[0].DataPort == 0 {
		t.Fatalf("response: %v %+v", err, content)
	}
	drainControl(r.player)
}
