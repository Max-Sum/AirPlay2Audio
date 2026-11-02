package zeroconf

// Changes to github.com/grandcat/zeroconf v1.0.0 for AirPlay2Audio:
//
//   - Questions for the host name (A, AAAA) are answered. The original
//     answers only service questions, so a sender whose cached address
//     records expired could no longer reach the receiver.
//   - On Windows, golang.org/x/net cannot tell which interface a multicast
//     packet arrived on, so the interface is found from the sender's
//     address, and otherwise every interface's addresses are used. Before,
//     answers there carried no addresses at all.
//   - Names are compared without regard to case or DNS escaping, so the
//     RAOP instance name (which contains "@", escaped by miekg/dns) is
//     answered.

import (
	"net"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// addrs returns the addresses to publish on the interface with index
// ifIndex: the configured ones, else those of that interface, else those of
// every interface the server uses.
func (s *Server) addrs(ifIndex int) (v4, v6 []net.IP) {
	v4 = s.service.AddrIPv4
	v6 = s.service.AddrIPv6
	if len(v4) > 0 || len(v6) > 0 {
		return v4, v6
	}
	if ifIndex != 0 {
		if iface, err := net.InterfaceByIndex(ifIndex); err == nil && iface != nil {
			v4, v6 = addrsForInterface(iface)
			if len(v4) > 0 || len(v6) > 0 {
				return v4, v6
			}
		}
	}
	for i := range s.ifaces {
		a4, a6 := addrsForInterface(&s.ifaces[i])
		v4 = append(v4, a4...)
		v6 = append(v6, a6...)
	}
	return v4, v6
}

// interfaceFor returns the index of the interface that from is reachable
// on: the zone of a link-local address, or the interface whose subnet holds
// the address. It returns 0 when unknown.
func (s *Server) interfaceFor(from net.Addr) int {
	addr, ok := from.(*net.UDPAddr)
	if !ok || addr == nil {
		return 0
	}
	if addr.Zone != "" {
		if index, err := strconv.Atoi(addr.Zone); err == nil {
			return index
		}
		if iface, err := net.InterfaceByName(addr.Zone); err == nil {
			return iface.Index
		}
	}
	for _, iface := range s.ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.Contains(addr.IP) {
				return iface.Index
			}
		}
	}
	return 0
}

// composeHostAnswers answers a question for the host name: the records of
// the asked type as answers, and the other address family as additional
// records (RFC 6762 section 6.2).
func (s *Server) composeHostAnswers(resp *dns.Msg, qtype uint16, ifIndex int) {
	for _, rr := range s.appendAddrs(nil, s.ttl, ifIndex, true) {
		if qtype == dns.TypeANY || rr.Header().Rrtype == qtype {
			resp.Answer = append(resp.Answer, rr)
		} else {
			resp.Extra = append(resp.Extra, rr)
		}
	}
}

// sameName compares two domain names ignoring case, escaping and the
// trailing dot.
func sameName(a, b string) bool {
	return strings.EqualFold(unescapeName(a), unescapeName(b))
}

func unescapeName(name string) string {
	name = strings.TrimSuffix(name, ".")
	if !strings.Contains(name, `\`) {
		return name
	}
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c != '\\' || i+1 >= len(name) {
			b.WriteByte(c)
			continue
		}
		// \DDD is a decimal byte value; \X is X.
		if i+3 < len(name) && isDigit(name[i+1]) && isDigit(name[i+2]) && isDigit(name[i+3]) {
			if v, err := strconv.Atoi(name[i+1 : i+4]); err == nil && v < 256 {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(name[i+1])
		i++
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
