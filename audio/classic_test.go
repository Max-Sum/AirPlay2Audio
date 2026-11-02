package audio

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"net"
	"strconv"
	"testing"
	"time"

	"goplay2/globals"
	"goplay2/ntp"
	"goplay2/ptp"

	alac "github.com/mycophonic/saprobe-alac"
)

func uncompressedALAC() []byte {
	w := &testBitWriter{}
	w.put(1, 3)
	w.put(0, 4)
	w.put(0, 12)
	w.put(1, 4) // stereo, escape/uncompressed
	for i := 0; i < 352; i++ {
		w.put(uint64(uint16(i)), 16)
		w.put(uint64(uint16(1000+i)), 16)
	}
	w.put(7, 3) // end of frame
	return w.data
}

var classicALAC = alac.PacketConfig{FrameLength: 352, BitDepth: 16, NumChannels: 2, PB: 40, MB: 10, KB: 14, MaxRun: 255, SampleRate: 44100}

func TestClassicDecryptsAndDecodesALAC(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")
	payload := uncompressedALAC()
	encrypted := append([]byte(nil), payload...)
	block, _ := aes.NewCipher(key)
	n := len(encrypted) &^ 15
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted[:n], encrypted[:n])
	header := make([]byte, 12)
	header[0], header[1] = 0x80, 0xe0 // marker set on the first packet
	binary.BigEndian.PutUint16(header[2:4], 7)
	binary.BigEndian.PutUint32(header[4:8], 5000)
	decode, err := classicDecoder(ClassicFormat{Codec: "alac", ALAC: classicALAC, Key: key, IV: iv})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := decode(append(header, encrypted...))
	if err != nil {
		t.Fatal(err)
	}
	if frame.Timestamp != 5000 || frame.SequenceNumber != 7 || len(frame.Data()) != 352*4 {
		t.Fatalf("frame ts=%d seq=%d len=%d", frame.Timestamp, frame.SequenceNumber, len(frame.Data()))
	}
	if binary.LittleEndian.Uint16(frame.Data()[4:6]) != 1 || binary.LittleEndian.Uint16(frame.Data()[6:8]) != 1001 {
		t.Fatal("decoded samples differ")
	}
}

func TestClassicL16IsByteSwapped(t *testing.T) {
	decode, err := classicDecoder(ClassicFormat{Codec: "l16"})
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 12)
	header[0], header[1] = 0x80, 0x60
	frame, err := decode(append(header, 0x12, 0x34, 0xff, 0xfe))
	if err != nil {
		t.Fatal(err)
	}
	if got := frame.Data(); got[0] != 0x34 || got[1] != 0x12 || got[2] != 0xfe || got[3] != 0xff {
		t.Fatalf("got %x", got)
	}
}

func TestClassicSyncAnchorsWithReceiverLatency(t *testing.T) {
	packet := make([]byte, 20)
	packet[0], packet[1] = 0x90, 0xd4
	binary.BigEndian.PutUint32(packet[4:8], 100000)
	ntp.PutNTP(packet[8:16], 3_900_000_000*int64(time.Second))
	binary.BigEndian.PutUint32(packet[16:20], 100000+77175)
	stamp, networkTime, latency := parseClassicSync(packet)
	if stamp != 100000-11025 || latency != 77175 || networkTime != 3_900_000_000*int64(time.Second) {
		t.Fatalf("stamp=%d latency=%d time=%d", stamp, latency, networkTime)
	}
}

// End to end over UDP: an unencrypted session receives audio and a sync
// packet, and asks the sender's control port to resend a missing packet.
func TestClassicSessionOverUDP(t *testing.T) {
	player := NewPlayer(ptp.NewVirtualClock(0), NewRing(8))
	server, _ := NewServer(player)
	senderControl, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer senderControl.Close()
	senderTiming, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer senderTiming.Close()
	peer := ClassicPeer{Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, ControlPort: senderControl.LocalAddr().(*net.UDPAddr).Port, TimingPort: senderTiming.LocalAddr().(*net.UDPAddr).Port}
	dataPort, controlPort, timingPort, err := server.SetupClassic(ClassicFormat{Codec: "alac", ALAC: classicALAC}, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Teardown()
	if timingPort == 0 {
		t.Fatal("no timing port")
	}
	// The receiver asks the sender for the time.
	senderTiming.SetReadDeadline(time.Now().Add(2 * time.Second))
	request := make([]byte, 64)
	if n, _, err := senderTiming.ReadFromUDP(request); err != nil || n != 32 || request[1] != 0xd2 {
		t.Fatalf("no timing request: n=%d err=%v", n, err)
	}
	data, _ := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(dataPort)))
	defer data.Close()
	control, _ := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(controlPort)))
	defer control.Close()
	send := func(seq uint16) {
		header := make([]byte, 12)
		header[0], header[1] = 0x80, 0x60
		binary.BigEndian.PutUint16(header[2:4], seq)
		binary.BigEndian.PutUint32(header[4:8], uint32(seq)*352)
		data.Write(append(header, uncompressedALAC()...))
	}
	sync := make([]byte, 20)
	sync[0], sync[1] = 0x90, 0xd4
	binary.BigEndian.PutUint32(sync[4:8], 20000)
	binary.BigEndian.PutUint32(sync[16:20], 20000+77175)
	control.Write(sync)
	select {
	case msg := <-player.ControlChannel:
		if msg.MType != globals.ANCHOR || msg.Param2 != 20000-11025 {
			t.Fatalf("unexpected anchor %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no anchor from sync packet")
	}
	send(1)
	send(3) // 2 is missing
	senderControl.SetReadDeadline(time.Now().Add(2 * time.Second))
	resend := make([]byte, 64)
	n, _, err := senderControl.ReadFromUDP(resend)
	if err != nil || n != 8 || resend[1] != 0xd5 || binary.BigEndian.Uint16(resend[4:6]) != 2 || binary.BigEndian.Uint16(resend[6:8]) != 1 {
		t.Fatalf("resend request n=%d err=%v %x", n, err, resend[:n])
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if value, err := player.ringBuffer.TryPop(); err == nil {
			if value.(*PCMFrame).SequenceNumber != 1 {
				t.Fatalf("first frame seq %d", value.(*PCMFrame).SequenceNumber)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no decoded frame")
}
