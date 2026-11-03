package audio

import (
	"encoding/binary"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/ptp"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/brutella/hc/crypto/chacha20poly1305"
)

type testBitWriter struct {
	data []byte
	bits uint
}

func (w *testBitWriter) put(value uint64, count uint) {
	for n := count; n > 0; n-- {
		if w.bits%8 == 0 {
			w.data = append(w.data, 0)
		}
		if (value>>(n-1))&1 != 0 {
			w.data[len(w.data)-1] |= 1 << (7 - w.bits%8)
		}
		w.bits++
	}
}

func testALACPacket(key []byte) ([]byte, error) {
	w := &testBitWriter{}
	w.put(1, 3)
	w.put(0, 4)
	w.put(0, 12)
	w.put(1, 4) // stereo, escape/uncompressed
	for i := 0; i < 352; i++ {
		w.put(uint64(uint16(i)), 16)
		w.put(uint64(uint16(1000+i)), 16)
	}
	header := make([]byte, 12)
	header[0], header[1], header[3] = 0x80, 96, 9
	binary.BigEndian.PutUint32(header[4:8], 123456)
	binary.BigEndian.PutUint32(header[8:12], 0x1234)
	nonce := make([]byte, 8)
	ciphertext, tag, err := chacha20poly1305.EncryptAndSeal(key, nonce, w.data, header[4:12])
	if err != nil {
		return nil, err
	}
	return append(append(append(header, ciphertext...), tag[:]...), nonce...), nil
}

func TestRealtimeUDPDecryptDecodeAndAnchor(t *testing.T) {
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(4))
	server, err := NewServer(player)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	dataPort, controlPort, err := server.SetupRealtime(key, 352)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Teardown()
	packet, err := testALACPacket(key)
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
	// The first anchor of a session carries the sentinel bit (0x10); a
	// packet without it this early would be taken for a leftover.
	control[0], control[1] = 0x90, 0xd7
	binary.BigEndian.PutUint32(control[4:8], 20000)
	binary.BigEndian.PutUint64(control[8:16], 123456789)
	binary.BigEndian.PutUint32(control[16:20], 20000+77175)
	if _, err = controlConn.Write(control); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-player.ControlChannel:
		// Default (shairport) mode: frame_1 - 11025 plays at the network time.
		// The low-latency mode is covered by TestRealtimeAnchorModes.
		if msg.MType != globals.ANCHOR || msg.Param1 != 123456789 || msg.Param2 != 20000-11025 {
			t.Fatalf("unexpected anchor: %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no realtime PTP anchor")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if value, err := player.ringBuffer.TryPop(); err == nil {
			frame := value.(*PCMFrame)
			if frame.Timestamp != 123456 || len(frame.Data()) != 352*2 {
				t.Fatalf("bad ALAC output length or timestamp")
			}
			if frame.Data()[0] != 0 || frame.Data()[1] != 1000<<16 {
				t.Fatal("decoded channel samples differ")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no decrypted ALAC frame")
}

func TestRealtimeUDPReordersAuthenticatedPackets(t *testing.T) {
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(10))
	server, _ := NewServer(player)
	key := make([]byte, 32)
	port, _, err := server.SetupRealtime(key, 352)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Teardown()
	packet, err := testALACPacket(key)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, seq := range []uint16{9, 11, 10} {
		binary.BigEndian.PutUint16(packet[2:4], seq)
		if _, err := conn.Write(packet); err != nil {
			t.Fatal(err)
		}
	}
	var got []uint32
	deadline := time.Now().Add(2 * time.Second)
	for len(got) < 3 && time.Now().Before(deadline) {
		if value, err := player.ringBuffer.TryPop(); err == nil {
			got = append(got, value.(*PCMFrame).SequenceNumber)
		} else {
			time.Sleep(time.Millisecond)
		}
	}
	if len(got) != 3 || got[0] != 9 || got[1] != 10 || got[2] != 11 {
		t.Fatalf("unexpected packet order: %v", got)
	}
}

func TestRealtimeUDPRequestsAndAcceptsResentPacket(t *testing.T) {
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(10))
	server, _ := NewServer(player)
	key := make([]byte, 32)
	dataPort, controlPort, err := server.SetupRealtime(key, 352)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Teardown()
	packet, err := testALACPacket(key)
	if err != nil {
		t.Fatal(err)
	}
	// The sender's control socket: its first packet tells the receiver where
	// to send resend requests.
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiverControl := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: controlPort}
	if _, err = sender.WriteToUDP(make([]byte, 8), receiverControl); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	dataConn, err := net.Dial("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(dataPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer dataConn.Close()
	withSeq := func(seq uint16) []byte {
		copyOf := append([]byte(nil), packet...)
		binary.BigEndian.PutUint16(copyOf[2:4], seq)
		return copyOf
	}
	for _, seq := range []uint16{9, 11} {
		if _, err := dataConn.Write(withSeq(seq)); err != nil {
			t.Fatal(err)
		}
	}
	request := make([]byte, 64)
	sender.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := sender.ReadFromUDP(request)
	if err != nil {
		t.Fatalf("no resend request: %v", err)
	}
	if n != 8 || request[0] != 0x80 || request[1] != 0xd5 ||
		binary.BigEndian.Uint16(request[4:6]) != 10 || binary.BigEndian.Uint16(request[6:8]) != 1 {
		t.Fatalf("unexpected resend request % x", request[:n])
	}
	resent := append([]byte{0x80, 0xd6, 0, 1}, withSeq(10)...)
	if _, err = sender.WriteToUDP(resent, receiverControl); err != nil {
		t.Fatal(err)
	}
	var got []uint32
	deadline := time.Now().Add(2 * time.Second)
	for len(got) < 3 && time.Now().Before(deadline) {
		if value, err := player.ringBuffer.TryPop(); err == nil {
			got = append(got, value.(*PCMFrame).SequenceNumber)
		} else {
			time.Sleep(time.Millisecond)
		}
	}
	if len(got) != 3 || got[0] != 9 || got[1] != 10 || got[2] != 11 {
		t.Fatalf("unexpected packet order: %v", got)
	}
}

func TestLowLatencyQueueChangesOnlyForRealtimeSession(t *testing.T) {
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(16))
	player.SetSynchronized(false)
	server, _ := NewServer(player)
	key := make([]byte, 32)
	packet, err := testALACPacket(key)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Teardown()
	for i := 0; i < 3; i++ {
		port, _, err := server.SetupRealtime(key, 352)
		if err != nil {
			t.Fatal(err)
		}
		if got := player.ringBuffer.Capacity(); got != 126 {
			t.Fatalf("realtime capacity = %d, want 126 packets", got)
		}
		conn, err := net.Dial("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			for n := 0; n < 100; n++ {
				_, _ = conn.Write(packet)
			}
		}()
		if _, err := server.Setup(key, 0x400000); err != nil {
			t.Fatal(err)
		}
		<-finished
		conn.Close()
		time.Sleep(10 * time.Millisecond)
		if got := player.ringBuffer.Capacity(); got != 16 {
			t.Fatalf("AAC capacity = %d, want 16 packets", got)
		}
		if got := player.ringBuffer.Length(); got != 0 {
			t.Fatalf("old realtime session enqueued %d packets after AAC setup", got)
		}
	}
}
