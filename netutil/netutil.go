// Package netutil opens the receiver's sockets: IPv4 and IPv6 unless
// IPv4Only is set, and for audio data within a configurable port range so
// firewall rules can be written for it.
//
// Fixed ports are bound separately for IPv4 and IPv6. Windows lets a
// dual-stack socket bind a port another program holds for IPv4 only, after
// which that program silently receives all IPv4 traffic; binding IPv4 on its
// own reports the conflict.
package netutil

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
)

var (
	// IPv4Only restricts every socket to IPv4.
	IPv4Only bool
	// PortBase and PortRange limit audio data, control and timing sockets
	// (TCP and UDP) to PortBase..PortBase+PortRange-1. PortBase 0 lets the
	// system choose.
	PortBase, PortRange int

	mu   sync.Mutex
	next int
)

func network(kind string) string {
	if IPv4Only {
		return kind + "4"
	}
	return kind
}

// UDP returns "udp" or "udp4", for dialing.
func UDP() string { return network("udp") }

// ListenFixed listens for TCP on a fixed port, on IPv4 and, when available,
// IPv6.
func ListenFixed(port int) (net.Listener, error) {
	address := net.JoinHostPort("", strconv.Itoa(port))
	v4, err := net.Listen("tcp4", address)
	if err != nil || IPv4Only {
		return v4, err
	}
	v6, err := net.Listen("tcp6", address)
	if err != nil {
		return v4, nil // no IPv6 on this computer
	}
	return newMultiListener(v4, v6), nil
}

// ListenFixedUDP listens for UDP on a fixed port. v6 is nil without IPv6.
func ListenFixedUDP(port int) (v4, v6 *net.UDPConn, err error) {
	v4, err = net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil || IPv4Only {
		return v4, nil, err
	}
	v6, err = net.ListenUDP("udp6", &net.UDPAddr{Port: port})
	if err != nil {
		return v4, nil, nil
	}
	return v4, v6, nil
}

// try calls open with each port of the range in turn, starting after the
// last port handed out, until one succeeds.
func try[T any](open func(port int) (T, error)) (T, error) {
	if PortBase == 0 {
		return open(0)
	}
	size := PortRange
	if size <= 0 {
		size = 1
	}
	mu.Lock()
	start := next
	mu.Unlock()
	var zero T
	var last error
	for i := 0; i < size; i++ {
		port := PortBase + (start+i)%size
		value, err := open(port)
		if err == nil {
			mu.Lock()
			next = (start + i + 1) % size
			mu.Unlock()
			return value, nil
		}
		last = err
	}
	return zero, fmt.Errorf("no free port in %d-%d: %w", PortBase, PortBase+size-1, last)
}

// ListenUDP opens an audio UDP socket. A single dual-stack socket is used
// (its peer may use either protocol); for a port from the range, IPv4 is
// checked first as explained in the package comment.
func ListenUDP() (*net.UDPConn, error) {
	return try(func(port int) (*net.UDPConn, error) {
		if IPv4Only {
			return net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		}
		if port != 0 {
			probe, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
			if err != nil {
				return nil, err
			}
			probe.Close()
		}
		return net.ListenUDP("udp", &net.UDPAddr{Port: port})
	})
}

// ListenTCP opens an audio TCP listener, like ListenUDP.
func ListenTCP() (net.Listener, error) {
	return try(func(port int) (net.Listener, error) {
		address := net.JoinHostPort("", strconv.Itoa(port))
		if IPv4Only {
			return net.Listen("tcp4", address)
		}
		if port != 0 {
			probe, err := net.Listen("tcp4", address)
			if err != nil {
				return nil, err
			}
			probe.Close()
		}
		return net.Listen("tcp", address)
	})
}

// multiListener accepts from several listeners.
type multiListener struct {
	listeners []net.Listener
	accepted  chan accepted
	closed    chan struct{}
	once      sync.Once
}

type accepted struct {
	conn net.Conn
	err  error
}

func newMultiListener(listeners ...net.Listener) *multiListener {
	m := &multiListener{listeners: listeners, accepted: make(chan accepted), closed: make(chan struct{})}
	for _, l := range listeners {
		go func(l net.Listener) {
			for {
				conn, err := l.Accept()
				select {
				case m.accepted <- accepted{conn, err}:
				case <-m.closed:
					if conn != nil {
						conn.Close()
					}
					return
				}
				if err != nil {
					return
				}
			}
		}(l)
	}
	return m
}

func (m *multiListener) Accept() (net.Conn, error) {
	select {
	case a := <-m.accepted:
		return a.conn, a.err
	case <-m.closed:
		return nil, net.ErrClosed
	}
}

func (m *multiListener) Close() error {
	var errs []error
	m.once.Do(func() {
		close(m.closed)
		for _, l := range m.listeners {
			errs = append(errs, l.Close())
		}
	})
	return errors.Join(errs...)
}

func (m *multiListener) Addr() net.Addr { return m.listeners[0].Addr() }
