package ntp

import (
	"net"
	"testing"
	"time"

	"github.com/Max-Sum/AirPlay2Audio/ptp"
)

func TestTimestampRoundTrip(t *testing.T) {
	b := make([]byte, 8)
	ns := int64(3_900_000_000)*int64(time.Second) + 123_456_789
	PutNTP(b, ns)
	if got := FromNTP(b); got-ns > 1 || ns-got > 1 {
		t.Fatalf("got %d, want %d", got, ns)
	}
}

// A fake sender whose clock is 10 hours ahead answers timing requests; the
// virtual clock must follow it.
func TestClientFollowsSenderClock(t *testing.T) {
	sender, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	const ahead = 10 * time.Hour
	go func() {
		buffer := make([]byte, 64)
		for {
			n, from, err := sender.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if n < 32 || buffer[1]&0x7f != typeRequest {
				continue
			}
			now := time.Now().UnixNano() + int64(ahead)
			reply := make([]byte, 32)
			reply[0], reply[1] = 0x80, 0xd3
			copy(reply[8:16], buffer[24:32])
			PutNTP(reply[16:24], now)
			PutNTP(reply[24:32], now)
			sender.WriteToUDP(reply, from)
		}
	}()
	local, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clock := ptp.NewVirtualClock(0)
	client := Start(local, sender.LocalAddr().(*net.UDPAddr), clock)
	defer client.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !clock.Synchronized() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !clock.Synchronized() {
		t.Fatal("clock never synchronized")
	}
	got := clock.Now().UnixNano()
	want := time.Now().UnixNano() + int64(ahead)
	if diff := time.Duration(got - want); diff > 20*time.Millisecond || diff < -20*time.Millisecond {
		t.Fatalf("clock differs from sender by %v", diff)
	}
}
