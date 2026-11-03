package audio

import (
	"encoding/binary"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/Max-Sum/AirPlay2Audio/codec"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/ptp"
)

func TestRealtimeFormatFor(t *testing.T) {
	cases := []struct {
		format uint64
		rate   uint32
		want   RealtimeFormat
		ok     bool
	}{
		{0, 0, RealtimeFormat{44100, 16}, true},
		{0, 48000, RealtimeFormat{48000, 16}, true},
		{0, 96000, RealtimeFormat{}, false},
		{1 << 18, 0, RealtimeFormat{44100, 16}, true},
		{1 << 18, 44100, RealtimeFormat{44100, 16}, true},
		{1 << 19, 0, RealtimeFormat{44100, 24}, true},
		{1 << 20, 48000, RealtimeFormat{48000, 16}, true},
		{1 << 21, 48000, RealtimeFormat{48000, 24}, true},
		{1 << 18, 48000, RealtimeFormat{}, false}, // rate contradicts format
		{1 << 22, 0, RealtimeFormat{}, false},     // AAC
		{1<<18 | 1<<21, 0, RealtimeFormat{}, false},
	}
	for _, c := range cases {
		got, ok := RealtimeFormatFor(c.format, c.rate)
		if ok != c.ok || ok && got != c.want {
			t.Errorf("RealtimeFormatFor(%#x, %d) = %+v, %t; want %+v, %t", c.format, c.rate, got, ok, c.want, c.ok)
		}
	}
	for _, bit := range []uint64{1 << 18, 1 << 19, 1 << 20, 1 << 21} {
		if RealtimeFormats&bit == 0 {
			t.Errorf("realtime format %#x not advertised", bit)
		}
	}
}

// At 48 kHz the 250 ms anchor allowance is 12000 samples.
func TestRealtimeAnchorLeadFollowsRate(t *testing.T) {
	if got := realtimeAnchorStamp(100000, 100000+84000, AnchorShairport, 48000); got != 100000-12000 {
		t.Fatalf("48 kHz shairport mode = %d, want frame_1 - 12000", got)
	}
	if got := realtimeAnchorStamp(100000, 100000+84000, AnchorLowLatency, 48000); got != 100000+84000-12000 {
		t.Fatalf("48 kHz low-latency mode = %d, want frame_2 - 12000", got)
	}
}

// A 48 kHz 24-bit realtime stream is decoded at 24 bits, tagged with its
// format, and anchored with a 48 kHz allowance.
func TestRealtime48kHz24BitDecodeAndAnchor(t *testing.T) {
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(4))
	server, err := NewServer(player)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	format := RealtimeFormat{Rate: 48000, Bits: 24}
	dataPort, controlPort, err := server.SetupRealtime(key, 352, format)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Teardown()
	packet, err := testALACPacketBits(key, 24)
	if err != nil {
		t.Fatal(err)
	}
	dataConn, err := net.Dial("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(dataPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer dataConn.Close()
	if _, err = dataConn.Write(packet); err != nil {
		t.Fatal(err)
	}
	controlConn, err := net.Dial("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(controlPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer controlConn.Close()
	control := make([]byte, 28)
	control[0], control[1] = 0x90, 0xd7
	binary.BigEndian.PutUint32(control[4:8], 20000)
	binary.BigEndian.PutUint64(control[8:16], 123456789)
	binary.BigEndian.PutUint32(control[16:20], 20000+84000)
	if _, err = controlConn.Write(control); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-player.ControlChannel:
		if msg.MType != globals.ANCHOR || msg.Param2 != 20000-12000 {
			t.Fatalf("unexpected anchor: %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no realtime PTP anchor")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if value, err := player.ringBuffer.TryPop(); err == nil {
			frame := value.(*PCMFrame)
			if frame.Format() != (codec.Format{Rate: 48000, Bits: 24}) {
				t.Fatalf("frame format = %+v", frame.Format())
			}
			if len(frame.Data()) != 352*2 {
				t.Fatalf("frame length = %d", len(frame.Data()))
			}
			if frame.Data()[2] != 1<<8 || frame.Data()[3] != 1001<<8 {
				t.Fatalf("24-bit samples = %d %d", frame.Data()[2], frame.Data()[3])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no decoded 24-bit frame")
}
