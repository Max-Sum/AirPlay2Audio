// A local AirPlay 1 (RAOP) protocol exerciser, not a real sender. It checks
// the Apple-Challenge answer, announces an RSA/AES-encrypted ALAC stream,
// answers NTP timing requests and sends silent audio with sync packets to an
// already running receiver.
//
//	go run ./tools/mock_raop -host 127.0.0.1 -port 7000 -seconds 5
package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Max-Sum/AirPlay2Audio/handlers"
	"github.com/Max-Sum/AirPlay2Audio/ntp"
)

type client struct {
	conn     net.Conn
	reader   *bufio.Reader
	sequence int
}

func (c *client) request(method, url string, headers map[string]string, body string) (map[string]string, error) {
	c.sequence++
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\nCSeq: %d\r\n", method, url, c.sequence)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if body != "" {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n" + body)
	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		return nil, err
	}
	status, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	response := map[string]string{}
	size := 0
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			response[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			if strings.EqualFold(k, "Content-Length") {
				size, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
	}
	if size > 0 {
		c.reader.Discard(size)
	}
	if !strings.Contains(status, " 200 ") {
		return response, fmt.Errorf("%s: %s", method, strings.TrimSpace(status))
	}
	return response, nil
}

// silentALAC is one uncompressed 352-sample stereo ALAC frame of silence.
func silentALAC() []byte {
	var bits []byte
	n := 0
	put := func(value uint64, count int) {
		for i := count - 1; i >= 0; i-- {
			if n%8 == 0 {
				bits = append(bits, 0)
			}
			if value>>uint(i)&1 != 0 {
				bits[len(bits)-1] |= 1 << (7 - n%8)
			}
			n++
		}
	}
	put(1, 3)
	put(0, 4)
	put(0, 12)
	put(1, 4)
	for i := 0; i < 352*2; i++ {
		put(0, 16)
	}
	put(7, 3)
	return bits
}

func main() {
	host := flag.String("host", "127.0.0.1", "receiver address")
	port := flag.Int("port", 7000, "receiver RTSP port")
	seconds := flag.Int("seconds", 5, "seconds of audio to send")
	flag.Parse()
	if err := run(*host, *port, *seconds); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func run(host string, port, seconds int) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	c := &client{conn: conn, reader: bufio.NewReader(conn)}
	public, err := handlers.AirPlay1PublicKey()
	if err != nil {
		return err
	}

	challenge := make([]byte, 16)
	rand.Read(challenge)
	resp, err := c.request("OPTIONS", "*", map[string]string{"Apple-Challenge": base64.RawStdEncoding.EncodeToString(challenge)}, "")
	if err != nil {
		return err
	}
	signature, err := base64.RawStdEncoding.DecodeString(resp["apple-response"])
	if err != nil {
		return fmt.Errorf("Apple-Response: %w", err)
	}
	// The receiver signs its own address on this connection and its device
	// ID; only check that the signature opens with the public key and
	// starts with the challenge.
	m := new(big.Int).Exp(new(big.Int).SetBytes(signature), big.NewInt(int64(public.E)), public.N).FillBytes(make([]byte, 256))
	if !strings.Contains(string(m), string(challenge)) {
		return fmt.Errorf("Apple-Response does not contain the challenge")
	}
	fmt.Println("OPTIONS: Apple-Response verified")

	key, iv := make([]byte, 16), make([]byte, 16)
	rand.Read(key)
	rand.Read(iv)
	encrypted, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, public, key, nil)
	if err != nil {
		return err
	}
	local := conn.LocalAddr().(*net.TCPAddr).IP
	urlHost := local.String()
	if local.To4() == nil {
		urlHost = "[" + urlHost + "]"
	}
	url := "rtsp://" + urlHost + "/1234567890"
	sdp := "v=0\r\no=mock 1234567890 0 IN IP4 " + local.String() + "\r\ns=mock\r\nt=0 0\r\nm=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 AppleLossless\r\na=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\r\n" +
		"a=rsaaeskey:" + base64.RawStdEncoding.EncodeToString(encrypted) + "\r\na=aesiv:" + base64.RawStdEncoding.EncodeToString(iv) + "\r\n"
	if _, err = c.request("ANNOUNCE", url, map[string]string{"Content-Type": "application/sdp", "X-Apple-Client-Name": "mock_raop"}, sdp); err != nil {
		return err
	}
	timing, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return err
	}
	defer timing.Close()
	control, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return err
	}
	defer control.Close()
	resp, err = c.request("SETUP", url, map[string]string{"Transport": fmt.Sprintf("RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;control_port=%d;timing_port=%d",
		control.LocalAddr().(*net.UDPAddr).Port, timing.LocalAddr().(*net.UDPAddr).Port)}, "")
	if err != nil {
		return err
	}
	ports := map[string]int{}
	for _, part := range strings.Split(resp["transport"], ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			ports[k], _ = strconv.Atoi(v)
		}
	}
	fmt.Printf("SETUP: %s\n", resp["transport"])
	if _, err = c.request("RECORD", url, map[string]string{"RTP-Info": "seq=0;rtptime=0", "Range": "npt=0-"}, ""); err != nil {
		return err
	}
	if _, err = c.request("SET_PARAMETER", url, map[string]string{"Content-Type": "text/parameters"}, "volume: -144.0\r\n"); err != nil {
		return err
	}

	// The sender clock: an arbitrary origin, as real senders use.
	origin := time.Now()
	senderNow := func() int64 { return int64(time.Since(origin)) + 1000*int64(time.Second) }
	timingRequests := 0
	go func() {
		buffer := make([]byte, 64)
		for {
			n, from, err := timing.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if n < 32 || buffer[1]&0x7f != 0x52 {
				continue
			}
			timingRequests++
			reply := make([]byte, 32)
			reply[0], reply[1] = 0x80, 0xd3
			copy(reply[8:16], buffer[24:32])
			ntp.PutNTP(reply[16:24], senderNow())
			ntp.PutNTP(reply[24:32], senderNow())
			timing.WriteToUDP(reply, from)
		}
	}()

	receiverIP := conn.RemoteAddr().(*net.TCPAddr).IP
	data := &net.UDPAddr{IP: receiverIP, Port: ports["server_port"]}
	controlAddr := &net.UDPAddr{IP: receiverIP, Port: ports["control_port"]}
	block, _ := aes.NewCipher(key)
	frame := silentALAC()
	const latency = 77175
	start := time.Now()
	var rtpTime uint32 = 1000
	for seq := uint16(0); time.Since(start) < time.Duration(seconds)*time.Second; seq++ {
		// Sync once per second: the frame now playing is rtpTime-latency.
		if seq%125 == 0 {
			sync := make([]byte, 20)
			sync[0], sync[1] = 0x80, 0xd4
			if seq == 0 {
				sync[0] = 0x90
			}
			binary.BigEndian.PutUint32(sync[4:8], rtpTime-latency)
			ntp.PutNTP(sync[8:16], senderNow())
			binary.BigEndian.PutUint32(sync[16:20], rtpTime)
			control.WriteToUDP(sync, controlAddr)
		}
		payload := append([]byte(nil), frame...)
		n := len(payload) &^ 15
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(payload[:n], payload[:n])
		header := make([]byte, 12)
		header[0], header[1] = 0x80, 0x60
		binary.BigEndian.PutUint16(header[2:4], seq)
		binary.BigEndian.PutUint32(header[4:8], rtpTime)
		control.WriteToUDP(append(header, payload...), data)
		rtpTime += 352
		time.Sleep(time.Until(start.Add(time.Duration(seq+1) * 352 * time.Second / 44100)))
	}
	if timingRequests == 0 {
		return fmt.Errorf("receiver never asked for the time")
	}
	fmt.Printf("sent %d s of audio; answered %d timing requests\n", seconds, timingRequests)
	_, err = c.request("TEARDOWN", url, nil, "")
	return err
}
