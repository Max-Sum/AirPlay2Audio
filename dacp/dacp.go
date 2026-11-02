// Package dacp sends remote control commands (next track, pause, ...) to an
// AirPlay 1 sender, as shairport-sync's dacp.c does. The sender names itself
// in the DACP-ID and Active-Remote request headers; its control port is
// found by looking up "iTunes_Ctrl_<DACP-ID>" in the _dacp._tcp mDNS service,
// and commands are HTTP GET /ctrl-int/1/<command> requests carrying the
// Active-Remote value.
package dacp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Max-Sum/AirPlay2Audio/internal/zeroconf"
)

// Commands, by the MediaRemote numbers used on the AirPlay 2 event channel
// (event.CommandPlay and so on).
var commandPaths = map[uint64]string{
	0: "play",
	1: "pause",
	2: "playpause",
	3: "stop",
	4: "nextitem",
	5: "previtem",
}

const (
	lookupTimeout  = 3 * time.Second
	requestTimeout = 3 * time.Second
	// A sender may restart its remote control server on another port.
	portLifetime = 10 * time.Minute
)

// Remote is one sender's remote control server.
type Remote struct {
	host, id, activeRemote string

	mu       sync.Mutex
	port     int
	resolved time.Time

	// lookup finds the control port; replaced in tests.
	lookup func(ctx context.Context, id string) (int, error)
	client *http.Client
}

// New returns the remote control of the sender at host.
func New(host, id, activeRemote string) *Remote {
	return &Remote{host: host, id: id, activeRemote: activeRemote, lookup: lookupPort, client: &http.Client{Timeout: requestTimeout}}
}

// ID returns the sender's DACP-ID.
func (r *Remote) ID() string { return r.id }

// Command sends a MediaRemote command number.
func (r *Remote) Command(command uint64) error {
	path, ok := commandPaths[command]
	if !ok {
		return fmt.Errorf("command %d has no DACP equivalent", command)
	}
	return r.Send(path)
}

// Send performs one DACP control command, such as "nextitem".
func (r *Remote) Send(path string) error {
	port, err := r.controlPort()
	if err != nil {
		return err
	}
	url := "http://" + net.JoinHostPort(r.host, strconv.Itoa(port)) + "/ctrl-int/1/" + path
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Active-Remote", r.activeRemote)
	response, err := r.client.Do(request)
	if err != nil {
		r.forgetPort()
		return fmt.Errorf("DACP %s: %w", path, err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DACP %s: status %d", path, response.StatusCode)
	}
	return nil
}

func (r *Remote) controlPort() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.port != 0 && time.Since(r.resolved) < portLifetime {
		return r.port, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	port, err := r.lookup(ctx, r.id)
	if err != nil {
		return 0, err
	}
	r.port, r.resolved = port, time.Now()
	return port, nil
}

func (r *Remote) forgetPort() {
	r.mu.Lock()
	r.port = 0
	r.mu.Unlock()
}

func lookupPort(ctx context.Context, id string) (int, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return 0, err
	}
	entries := make(chan *zeroconf.ServiceEntry, 4)
	if err := resolver.Lookup(ctx, "iTunes_Ctrl_"+id, "_dacp._tcp", "local.", entries); err != nil {
		return 0, err
	}
	for {
		select {
		case entry, ok := <-entries:
			if !ok {
				return 0, fmt.Errorf("DACP remote iTunes_Ctrl_%s not found", id)
			}
			if entry != nil && entry.Port > 0 {
				return entry.Port, nil
			}
		case <-ctx.Done():
			return 0, fmt.Errorf("DACP remote iTunes_Ctrl_%s not found", id)
		}
	}
}
