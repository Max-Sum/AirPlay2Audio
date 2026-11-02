// Package ntp implements the AirPlay 1 timing exchange. It resembles NTP:
// the receiver sends timing requests to the sender's timing port and the
// sender answers with its own clock; senders may also ask the receiver.
// Times on the wire are 64-bit NTP timestamps (seconds since 1900 and a
// 32-bit fraction). The sender's clock is arbitrary, not wall time.
package ntp

import (
	"encoding/binary"
	"log"
	"net"
	"sync"
	"time"

	"goplay2/ptp"
)

const (
	typeRequest = 0x52
	typeReply   = 0x53
	packetSize  = 32
	// Keep the best of this many recent exchanges: the one with the
	// shortest round trip has the least queueing error.
	window = 8
)

// FromNTP converts a 64-bit NTP timestamp to nanoseconds since 1900, the
// unit used on the virtual clock while an AirPlay 1 sender is followed.
func FromNTP(b []byte) int64 {
	seconds := int64(binary.BigEndian.Uint32(b[0:4]))
	fraction := int64(binary.BigEndian.Uint32(b[4:8]))
	return seconds*int64(time.Second) + fraction*int64(time.Second)>>32
}

// PutNTP writes nanoseconds as a 64-bit NTP timestamp.
func PutNTP(b []byte, ns int64) {
	seconds := ns / int64(time.Second)
	rest := ns % int64(time.Second)
	binary.BigEndian.PutUint32(b[0:4], uint32(seconds))
	binary.BigEndian.PutUint32(b[4:8], uint32((rest<<32)/int64(time.Second)))
}

type sample struct {
	offset time.Duration // sender minus local
	rtt    time.Duration
}

// Client follows a sender's clock over a UDP socket until Close.
type Client struct {
	conn   *net.UDPConn
	remote *net.UDPAddr
	clock  *ptp.VirtualClock
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	recent []sample
	seq    uint16
	synced bool
	paused bool // another sender owns the clock; keep measuring only
}

// SetPaused stops or restarts adjusting the clock. On restart the best
// recent measurement is applied at once.
func (c *Client) SetPaused(paused bool) {
	c.mu.Lock()
	c.paused = paused
	offset, ok := c.bestLocked()
	c.mu.Unlock()
	if !paused && ok {
		c.clock.SetLocalOffset(-offset)
	}
}

// bestLocked returns the offset of the lowest round-trip sample.
func (c *Client) bestLocked() (time.Duration, bool) {
	if len(c.recent) == 0 {
		return 0, false
	}
	best := c.recent[0]
	for _, s := range c.recent[1:] {
		if s.rtt < best.rtt {
			best = s
		}
	}
	return best.offset, true
}

// Start begins timing against remote, adjusting clock. The socket belongs
// to the client and is closed by Close.
func Start(conn *net.UDPConn, remote *net.UDPAddr, clock *ptp.VirtualClock) *Client {
	c := &Client{conn: conn, remote: remote, clock: clock, done: make(chan struct{})}
	go c.receive()
	go c.request()
	return c
}

func (c *Client) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.conn.Close()
}

func (c *Client) request() {
	// A quick burst for the first estimate, then one every two seconds.
	delays := []time.Duration{0, 50 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond}
	for i := 0; ; i++ {
		delay := 2 * time.Second
		if i < len(delays) {
			delay = delays[i]
		}
		select {
		case <-c.done:
			return
		case <-time.After(delay):
		}
		packet := make([]byte, packetSize)
		packet[0], packet[1] = 0x80, 0x80|typeRequest
		c.mu.Lock()
		c.seq++
		binary.BigEndian.PutUint16(packet[2:4], c.seq)
		c.mu.Unlock()
		PutNTP(packet[24:32], c.clock.LocalNow().UnixNano())
		if _, err := c.conn.WriteToUDP(packet, c.remote); err != nil {
			select {
			case <-c.done:
			default:
				log.Printf("NTP timing request: %v", err)
			}
			return
		}
	}
}

func (c *Client) receive() {
	buffer := make([]byte, 128)
	for {
		n, from, err := c.conn.ReadFromUDP(buffer)
		local := c.clock.LocalNow().UnixNano()
		if err != nil {
			return
		}
		if n < packetSize || !from.IP.Equal(c.remote.IP) {
			continue
		}
		switch buffer[1] & 0x7f {
		case typeRequest:
			// Answer with the local clock, so a sender can measure us too.
			reply := make([]byte, packetSize)
			reply[0], reply[1] = 0x80, 0x80|typeReply
			copy(reply[2:4], buffer[2:4])
			copy(reply[8:16], buffer[24:32])
			PutNTP(reply[16:24], local)
			PutNTP(reply[24:32], c.clock.LocalNow().UnixNano())
			c.conn.WriteToUDP(reply, from)
		case typeReply:
			t1 := FromNTP(buffer[8:16])
			t2 := FromNTP(buffer[16:24])
			t3 := FromNTP(buffer[24:32])
			c.add(t1, t2, t3, local)
		}
	}
}

// add records one exchange: t1 and t4 are local send and receive times,
// t2 and t3 the sender's receive and transmit times.
func (c *Client) add(t1, t2, t3, t4 int64) {
	rtt := time.Duration((t4 - t1) - (t3 - t2))
	if rtt < 0 || rtt > time.Second {
		return
	}
	offset := time.Duration(((t2 - t1) + (t3 - t4)) / 2)
	c.mu.Lock()
	c.recent = append(c.recent, sample{offset, rtt})
	if len(c.recent) > window {
		c.recent = c.recent[1:]
	}
	best := c.recent[0]
	for _, s := range c.recent[1:] {
		if s.rtt < best.rtt {
			best = s
		}
	}
	if c.paused {
		c.mu.Unlock()
		return
	}
	first := !c.synced
	c.synced = true
	c.mu.Unlock()
	c.clock.SetLocalOffset(-best.offset)
	if first {
		log.Printf("NTP clock synchronized (round trip %.1f ms)", float64(best.rtt)/1e6)
	}
}
