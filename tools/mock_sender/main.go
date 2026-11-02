// A local protocol exerciser, not an Apple device or a pairing test. It sends
// synthetic PTP and encrypted AAC to an already running receiver at zero volume.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ptp "github.com/albanseurat/go-ptp"
	"github.com/brutella/hc/crypto/chacha20poly1305"
	"howett.net/plist"
)

type client struct {
	conn     net.Conn
	reader   *bufio.Reader
	sequence int
}

func (c *client) request(method, path, contentType string, body []byte) ([]byte, error) {
	c.sequence++
	_, err := fmt.Fprintf(c.conn, "%s %s RTSP/1.0\r\nCSeq: %d\r\nContent-Length: %d\r\n", method, path, c.sequence, len(body))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		if _, err = fmt.Fprintf(c.conn, "Content-Type: %s\r\n", contentType); err != nil {
			return nil, err
		}
	}
	if _, err = c.conn.Write(append([]byte("\r\n"), body...)); err != nil {
		return nil, err
	}
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.Contains(line, " 200 ") {
		return nil, fmt.Errorf("%s: %s", method, strings.TrimSpace(line))
	}
	size := 0
	for {
		line, err = c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Content-Length") {
			size, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
		}
	}
	response := make([]byte, size)
	_, err = io.ReadFull(c.reader, response)
	return response, err
}

func sendPTP(host string) error {
	event, err := net.Dial("udp4", net.JoinHostPort(host, "319"))
	if err != nil {
		return err
	}
	defer event.Close()
	general, err := net.Dial("udp4", net.JoinHostPort(host, "320"))
	if err != nil {
		return err
	}
	defer general.Close()
	for seq := uint16(1); seq <= 3; seq++ {
		origin := time.Now()
		header := ptp.Header{VersionPTP: ptp.Version2, ClockIdentity: 0x1122334455667788, PortNumber: 1, SequenceID: seq}
		header.MessageType = ptp.SyncMsgType
		syncMsg := &ptp.SyncMsg{Header: header, OriginTimestamp: origin}
		packet, err := syncMsg.MarshalBinary()
		if err != nil {
			return err
		}
		if _, err = event.Write(packet); err != nil {
			return err
		}
		header.MessageType = ptp.FollowUpMsgType
		follow := &ptp.FollowUpMsg{Header: header, PreciseOriginTimestamp: origin}
		packet, err = follow.MarshalBinary()
		if err != nil {
			return err
		}
		if _, err = general.Write(packet); err != nil {
			return err
		}
		time.Sleep(15 * time.Millisecond) // let the receiver emit its delay request
		header.MessageType = ptp.DelayRespMsgType
		header.MessageLength = ptp.HeaderLen + ptp.DelayRespPayloadLen
		response := &ptp.DelRespMsg{Header: header}
		packet, err = response.MarshalBinary()
		if err != nil {
			return err
		}
		now := time.Now()
		secs := uint64(now.Unix())
		packet[34], packet[35] = byte(secs>>40), byte(secs>>32)
		binary.BigEndian.PutUint32(packet[36:40], uint32(secs))
		binary.BigEndian.PutUint32(packet[40:44], uint32(now.Nanosecond()))
		if _, err = general.Write(packet); err != nil {
			return err
		}
		time.Sleep(30 * time.Millisecond)
	}
	return nil
}

func binaryPlist(value interface{}) ([]byte, error) { return plist.Marshal(value, plist.BinaryFormat) }

func run() error {
	host, port := "127.0.0.1", "7000"
	if len(os.Args) > 1 {
		host = os.Args[1]
	}
	if len(os.Args) > 2 {
		port = os.Args[2] // the receiver's -port
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	c := &client{conn: conn, reader: bufio.NewReader(conn)}
	first, _ := binaryPlist(map[string]interface{}{"timingProtocol": "PTP"})
	if _, err = c.request("SETUP", "/mock", "application/x-apple-binary-plist", first); err != nil {
		return err
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	second, _ := binaryPlist(map[string]interface{}{"streams": []interface{}{map[string]interface{}{"type": 103, "audioFormat": 0x400000, "ct": 3, "spf": 1024, "shk": key}}})
	reply, err := c.request("SETUP", "/mock", "application/x-apple-binary-plist", second)
	if err != nil {
		return err
	}
	var setup struct {
		Streams []struct {
			DataPort uint16 `plist:"dataPort"`
		} `plist:"streams"`
	}
	if _, err = plist.Unmarshal(reply, &setup); err != nil {
		return err
	}
	if len(setup.Streams) != 1 {
		return fmt.Errorf("missing audio port")
	}
	audio, err := net.DialTimeout("tcp4", net.JoinHostPort(host, strconv.Itoa(int(setup.Streams[0].DataPort))), 3*time.Second)
	if err != nil {
		return err
	}
	defer audio.Close()
	if err = sendPTP(host); err != nil {
		return err
	}
	if _, err = c.request("SET_PARAMETER", "/mock", "text/parameters", []byte("volume: -144.000000\r\n")); err != nil {
		return err
	}
	anchor := time.Now().Add(1200 * time.Millisecond)
	fraction := new(big.Int).Lsh(big.NewInt(int64(anchor.Nanosecond())), 64)
	fraction.Div(fraction, big.NewInt(int64(time.Second)))
	const rtpAnchor = uint32(500000)
	anchorBody, _ := binaryPlist(map[string]interface{}{"rate": 1, "rtpTime": rtpAnchor, "networkTimeSecs": uint64(anchor.Unix()), "networkTimeFrac": fraction.Uint64(), "networkTimeTimelineID": uint64(0x1122334455667788)})
	if _, err = c.request("SETRATEANCHORTIME", "/mock", "application/x-apple-binary-plist", anchorBody); err != nil {
		return err
	}
	fixture, err := os.Open(filepath.Join("audio", "testdata", "sine-aac-lc.bin"))
	if err != nil {
		return err
	}
	defer fixture.Close()
	count := 0
	for {
		var size uint16
		if err := binary.Read(fixture, binary.BigEndian, &size); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		data := make([]byte, size)
		if _, err = io.ReadFull(fixture, data); err != nil {
			return err
		}
		header := make([]byte, 12)
		header[0], header[3] = 0x80, byte(count)
		binary.BigEndian.PutUint32(header[4:8], rtpAnchor+uint32(count)*1024)
		binary.BigEndian.PutUint32(header[8:12], 0x12345678)
		nonce := make([]byte, 8)
		binary.BigEndian.PutUint64(nonce, uint64(count))
		ciphertext, tag, err := chacha20poly1305.EncryptAndSeal(key, nonce, data, header[4:12])
		if err != nil {
			return err
		}
		packet := append(append(append(header, ciphertext...), tag[:]...), nonce...)
		if err = binary.Write(audio, binary.BigEndian, uint16(len(packet)+2)); err != nil {
			return err
		}
		if _, err = audio.Write(packet); err != nil {
			return err
		}
		count++
	}
	time.Sleep(2 * time.Second)
	end, _ := binaryPlist(map[string]interface{}{})
	if _, err = c.request("TEARDOWN", "/mock", "application/x-apple-binary-plist", end); err != nil {
		return err
	}
	fmt.Printf("Sent %d encrypted AAC packets with synthetic PTP and zero volume\n", count)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
