package handlers

import (
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brutella/hc/hap/pair"
	"goplay2/audio"
	"goplay2/event"
	"goplay2/globals"
	"goplay2/homekit"
	"goplay2/pairing"
	"goplay2/rtsp"
	"howett.net/plist"
)

type Rstp struct {
	mu        sync.Mutex
	streams   map[string]*audio.Server
	owners    map[string]*rtsp.Conn // stream path -> connection that set it up
	transient map[string]*pair.SetupServerSession
	sessions  map[*rtsp.Conn]bool
	active    *rtsp.Conn // connection of the sender that owns the output
	senders   map[*rtsp.Conn]*senderInfo
	order     uint64
	auth      passwordAuth
	pairing   *pairing.Controller
	player    *audio.Player
	classic   map[*rtsp.Conn]*classicSession // AirPlay 1 sessions
	lastSeen  map[*rtsp.Conn]time.Time       // last request on each connection
	// verified holds connections that completed HomeKit pair-verify, that is
	// whose controller key is stored in the receiver's pairing database.
	verified map[*rtsp.Conn]bool

	// ClockSources is called with the addresses whose PTP clock may be
	// followed: the active sender, then the peers it announces.
	ClockSources func([]net.IP)
	// TimingPeers is called with every connected sender's addresses, so
	// their PTP clocks stay measured while another sender plays.
	TimingPeers func([]net.IP)
	// AnchorClock is called with the PTP clock a playing sender's anchor
	// refers to.
	AnchorClock func(uint64)
	// HardwareAddr is signed into AirPlay 1 Apple-Challenge responses.
	HardwareAddr net.HardwareAddr
	// EventPort is the AirPlay 2 event channel port given to senders.
	EventPort int
	// RefuseInterruption refuses a new sender while another sender's
	// session lasts. By default the new sender takes over.
	RefuseInterruption bool

	advertise  func()           // publishes changed TXT records; see SetAdvertise
	advertised *advertisedState // last published group/session state
}

func NewRstpHandler(dataDir string, player *audio.Player, password string) (*Rstp, error) {

	ctrl, err := pairing.NewController(dataDir)
	if err != nil {
		return nil, err
	}
	return &Rstp{
		streams:   make(map[string]*audio.Server),
		owners:    make(map[string]*rtsp.Conn),
		transient: make(map[string]*pair.SetupServerSession),
		sessions:  make(map[*rtsp.Conn]bool),
		classic:   make(map[*rtsp.Conn]*classicSession),
		auth:      newPasswordAuth(password),
		pairing:   ctrl,
		player:    player,
		EventPort: 60003,
	}, nil
}

func (r *Rstp) OnConnOpen(conn *rtsp.Conn) {
	conn.SetNetConn(newSecureConn(conn.NetConn(), homekit.Server.Context))
}

func (r *Rstp) OnRequest(conn *rtsp.Conn, request *rtsp.Request) {
	if request.Path == "feedback" {
		return
	}
	log.Printf("RTSP %s %s body=%d", request.Method, request.Path, len(request.Body))
	if globals.Trace {
		traceRequest(request)
	}
}

// traceRequest prints headers and a readable form of the body. Only used
// with -trace, for protocol debugging.
func traceRequest(request *rtsp.Request) {
	keys := make([]string, 0, len(request.Header))
	for key := range request.Header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		log.Printf("TRACE   %s: %s", key, strings.Join(request.Header[key], ", "))
	}
	if len(request.Body) == 0 {
		return
	}
	contentType := ""
	if value, ok := request.Header["Content-Type"]; ok && len(value) > 0 {
		contentType = strings.ToLower(value[0])
	}
	switch {
	case strings.HasPrefix(contentType, "text/"):
		log.Printf("TRACE   body: %q", string(request.Body))
	case strings.Contains(contentType, "plist"):
		var content interface{}
		if _, err := plist.Unmarshal(request.Body, &content); err == nil {
			log.Printf("TRACE   plist: %s", summarizePlist(content, 0))
		}
	}
}

// summarizePlist shows keys and scalar values, shortening binary blobs.
func summarizePlist(value interface{}, depth int) string {
	if depth > 4 {
		return "…"
	}
	switch v := value.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+summarizePlist(v[key], depth+1))
		}
		return "{" + strings.Join(parts, " ") + "}"
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, summarizePlist(item, depth+1))
		}
		return "[" + strings.Join(parts, " ") + "]"
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(v))
	case string:
		if len(v) > 80 {
			return fmt.Sprintf("%q…", v[:80])
		}
		return fmt.Sprintf("%q", v)
	default:
		return fmt.Sprint(v)
	}
}

func (r *Rstp) Handle(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.publishGroup()
	r.noteRemote(conn, req)
	if r.lastSeen == nil {
		r.lastSeen = make(map[*rtsp.Conn]time.Time)
	}
	r.lastSeen[conn] = time.Now()
	if needsPassword(req) {
		if resp, err := r.auth.check(conn, req); resp != nil || err != nil {
			return resp, err
		}
	}
	diagnoseRemoteControl(conn, req)
	if r.resumeSignal(conn, req) {
		log.Printf("SESSION %s from waiting sender %q: it is playing again", req.Method, r.sender(conn).name)
		r.startPlaying(conn)
	}
	if session := r.classic[conn]; session != nil {
		if resp, err := r.onClassic(conn, session, req); resp != nil || err != nil {
			return resp, err
		}
	}
	switch req.Method {
	case "OPTIONS":
		return r.OnOptions(conn, req)
	case "ANNOUNCE":
		return r.OnAnnounce(conn, req)
	case "GET":
		return r.OnGetWeb(req)
	case "POST":
		return r.OnPostWeb(conn, req)
	case "SETUP":
		return r.OnSetupWeb(conn, req)
	case "GET_PARAMETER":
		return r.OnGetParameterWeb(req)
	case "SET_PARAMETER":
		r.remember(conn, req)
		if !r.foreground(conn) {
			// Kept for when this sender plays again.
			return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
		}
		return r.OnSetParameterWeb(req)
	case "RECORD":
		return r.OnRecordWeb(req)
	case "SETPEERS":
		return r.OnSetPeerWeb(conn, req)
	case "SETPEERSX":
		return r.OnSetPeersX(conn, req)
	case "SETRATEANCHORTIME":
		return r.OnSetRateAnchorTime(conn, req)
	case "SETRATE":
		return r.OnSetRate(conn, req)
	case "FLUSHBUFFERED":
		return r.OnFlushBuffered(conn, req)
	case "FLUSH":
		return r.OnFlushRealtime(req)
	case "TEARDOWN":
		return r.OnTeardownWeb(conn, req)
	}
	return &rtsp.Response{StatusCode: rtsp.StatusNotImplemented}, nil
}

func (r *Rstp) setClockSources(addresses []string) {
	if r.ClockSources == nil {
		return
	}
	var ips []net.IP
	for _, address := range addresses {
		if ip := net.ParseIP(address); ip != nil {
			ips = append(ips, ip)
		}
	}
	if len(ips) > 0 {
		r.ClockSources(ips)
	}
}

// dropStream stops a stream and forgets it.
func (r *Rstp) dropStream(path string) {
	if s, found := r.streams[path]; found {
		s.Teardown()
	}
	delete(r.streams, path)
	delete(r.owners, path)
}

// The SESSION lines are read by the window to show who is connected.
func (r *Rstp) sessionStarted(conn *rtsp.Conn, name string) {
	if r.sessions == nil {
		r.sessions = make(map[*rtsp.Conn]bool)
	}
	r.sessions[conn] = true
	name = strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return -1
		}
		return c
	}, name)
	if len(name) > 64 {
		name = name[:64]
	}
	info := r.sender(conn)
	info.name = name
	r.order++
	info.order = r.order
	r.updateTimingPeers()
	if r.active != conn {
		// Connected next to the sender that is playing; it takes over the
		// output when it starts playing.
		log.Printf("SESSION connected sender=%q (waiting; %q is playing)", name, r.sender(r.active).name)
		return
	}
	resetPlaybackState()
	log.Printf("SESSION started sender=%q", name)
}

func (r *Rstp) OnConnClose(conn *rtsp.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.publishGroup()
	r.forgetRemote(conn)
	delete(r.transient, conn.NetConn().RemoteAddr().String())
	delete(r.classic, conn)
	delete(r.lastSeen, conn)
	r.auth.forget(conn)
	delete(r.verified, conn)
	// A sender that disconnects without TEARDOWN leaves nothing playing.
	for path, owner := range r.owners {
		if owner == conn {
			r.dropStream(path)
		}
	}
	if r.sessions[conn] {
		delete(r.sessions, conn)
		log.Print("SESSION ended")
	}
	delete(r.senders, conn)
	r.updateTimingPeers()
	if r.active == conn {
		r.active = nil
		// Another connected sender, if any, gets the output back. It was
		// paused, so it starts again only when its user presses play.
		if next := r.latestSession(); next != nil {
			r.activate(next, true)
		} else {
			event.SetActiveSender("")
		}
	}
}

// ExpireIdle ends sessions with no request and no audio, control or timing
// packet for longer than timeout, as shairport-sync's session_timeout does
// for a sender that vanished without closing its connection. AirPlay 2
// senders send feedback every few seconds even when paused.
func (r *Rstp) ExpireIdle(timeout time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for conn := range r.sessions {
		last := r.lastSeen[conn]
		for path, owner := range r.owners {
			if stream := r.streams[path]; owner == conn && stream != nil && stream.LastActivity().After(last) {
				last = stream.LastActivity()
			}
		}
		if !last.IsZero() && now.Sub(last) > timeout {
			log.Printf("SESSION timed out after %s without activity", timeout)
			// The connection's reader then ends the session in OnConnClose.
			conn.Close()
		}
	}
}

func (r *Rstp) OnResponse(conn *rtsp.Conn, resp *rtsp.Response) {
	if resp.StatusCode >= 400 || globals.Trace {
		log.Printf("RTSP response status %d", resp.StatusCode)
	}
}
