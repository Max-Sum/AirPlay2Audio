package netutil

import (
	"net"
	"strconv"
	"testing"
)

func TestPortRangeIsUsedInTurn(t *testing.T) {
	PortBase, PortRange = 47310, 3
	defer func() { PortBase, PortRange = 0, 0 }()
	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		conn, err := ListenUDP()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		port := conn.LocalAddr().(*net.UDPAddr).Port
		if port < 47310 || port > 47312 || seen[port] {
			t.Fatalf("port %d (seen %v)", port, seen)
		}
		seen[port] = true
	}
	if _, err := ListenUDP(); err == nil {
		t.Fatal("expected the full range to be reported")
	}
}

// Another program holding the port for IPv4 must be reported, not hidden
// by a dual-stack bind.
func TestFixedPortConflictIsReported(t *testing.T) {
	other, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, _, err := ListenFixedUDP(other.LocalAddr().(*net.UDPAddr).Port); err == nil {
		t.Fatal("UDP conflict not reported")
	}
	tcp, err := net.Listen("tcp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	if _, err := ListenFixed(tcp.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("TCP conflict not reported")
	}
}

func TestFixedListenerAcceptsIPv4AndIPv6(t *testing.T) {
	probe, _ := net.Listen("tcp4", ":0")
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	l, err := ListenFixed(port)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, address := range []string{"127.0.0.1", "::1"} {
		c, err := net.Dial("tcp", net.JoinHostPort(address, strconv.Itoa(port)))
		if err != nil {
			if address == "::1" {
				t.Log("no IPv6 loopback:", err)
				continue
			}
			t.Fatal(err)
		}
		conn, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		c.Close()
	}
}
