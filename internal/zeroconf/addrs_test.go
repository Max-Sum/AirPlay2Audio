package zeroconf

import (
	"net"
	"testing"

	"github.com/miekg/dns"
)

func testServer(ips ...string) *Server {
	entry := NewServiceEntry("003111061AC1@ThinkCenter", "_raop._tcp", "local.")
	entry.HostName = "airplay2-66d988dfe4f1.local."
	entry.Port = 7000
	for _, ip := range ips {
		if parsed := net.ParseIP(ip); parsed.To4() != nil {
			entry.AddrIPv4 = append(entry.AddrIPv4, parsed)
		} else {
			entry.AddrIPv6 = append(entry.AddrIPv6, parsed)
		}
	}
	return &Server{service: entry, ttl: 3200}
}

// A question as a sender sends it, packed and unpacked by miekg/dns.
func question(t *testing.T, name string, qtype uint16) (*dns.Msg, dns.Question) {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(name, qtype)
	buf, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	var msg dns.Msg
	if err := msg.Unpack(buf); err != nil {
		t.Fatal(err)
	}
	return &msg, msg.Question[0]
}

func TestHostAddressQuestionsAreAnswered(t *testing.T) {
	s := testServer("192.168.0.11", "2406:3003:2003:7dd6::11")
	for _, tc := range []struct {
		qtype       uint16
		answer      uint16
		extra       uint16
		answerCount int
	}{
		{dns.TypeA, dns.TypeA, dns.TypeAAAA, 1},
		{dns.TypeAAAA, dns.TypeAAAA, dns.TypeA, 1},
	} {
		query, q := question(t, "airplay2-66d988dfe4f1.local.", tc.qtype)
		resp := new(dns.Msg)
		if err := s.handleQuestion(q, resp, query, 0); err != nil {
			t.Fatal(err)
		}
		if len(resp.Answer) != tc.answerCount || resp.Answer[0].Header().Rrtype != tc.answer {
			t.Fatalf("%s: answers %v", dns.TypeToString[tc.qtype], resp.Answer)
		}
		if len(resp.Extra) != 1 || resp.Extra[0].Header().Rrtype != tc.extra {
			t.Fatalf("%s: extra %v", dns.TypeToString[tc.qtype], resp.Extra)
		}
	}
}

func TestInstanceNameWithAtSignIsAnswered(t *testing.T) {
	s := testServer("192.168.0.11")
	query, q := question(t, "003111061AC1@ThinkCenter._raop._tcp.local.", dns.TypeSRV)
	resp := new(dns.Msg)
	if err := s.handleQuestion(q, resp, query, 0); err != nil {
		t.Fatal(err)
	}
	var srv, a bool
	for _, rr := range resp.Answer {
		switch rr.Header().Rrtype {
		case dns.TypeSRV:
			srv = true
		case dns.TypeA:
			a = true
		}
	}
	if !srv || !a {
		t.Fatalf("q.Name=%q answers %v", q.Name, resp.Answer)
	}
}

func TestAddressesWithoutInterfaceUseAllInterfaces(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skip(err)
	}
	s := testServer()
	s.ifaces = ifaces
	var want int
	for i := range ifaces {
		v4, v6 := addrsForInterface(&ifaces[i])
		want += len(v4) + len(v6)
	}
	if want == 0 {
		t.Skip("no interface addresses")
	}
	if got := s.appendAddrs(nil, 3200, 0, false); len(got) != want {
		t.Fatalf("got %d address records, want %d", len(got), want)
	}
}

func TestInterfaceForSenderAddress(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skip(err)
	}
	s := &Server{ifaces: ifaces}
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			got := s.interfaceFor(&net.UDPAddr{IP: ipnet.IP, Port: 5353})
			if got != iface.Index {
				t.Fatalf("interfaceFor(%s) = %d, want %d", ipnet.IP, got, iface.Index)
			}
			return
		}
	}
	t.Skip("no IPv4 interface")
}

func TestSameName(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{`003111061AC1\@ThinkCenter._raop._tcp.local.`, "003111061AC1@ThinkCenter._raop._tcp.local.", true},
		{`My\032Speaker._airplay._tcp.local.`, "My Speaker._airplay._tcp.local.", true},
		{"ThinkCenter._AirPlay._tcp.local", "thinkcenter._airplay._tcp.local.", true},
		{"Other._airplay._tcp.local.", "ThinkCenter._airplay._tcp.local.", false},
	} {
		if got := sameName(tc.a, tc.b); got != tc.want {
			t.Errorf("sameName(%q, %q) = %t", tc.a, tc.b, got)
		}
	}
}
