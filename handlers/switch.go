package handlers

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"goplay2/config"
	"goplay2/event"
	"goplay2/globals"
	"goplay2/homekit"
	"goplay2/rtsp"
)

// Several senders can stay connected at the same time. One of them, the
// active sender, owns the output; the others keep their sessions but their
// audio is held back. When another sender starts playing it becomes the
// active sender and the previous one is asked to pause (AirPlay 2, over its
// event channel). The previous sender's session stays open, so pressing
// play on it later switches back the same way.

// senderInfo is what the receiver keeps about each connected sender, so
// the window can show it again when the sender becomes active.
type senderInfo struct {
	name      string
	order     uint64 // session start order; higher is newer
	peers     []string
	volume    float64
	hasVolume bool
	track     []byte // last DMAP track info
	artType   string
	art       []byte
	hasArt    bool
	pausedAt  time.Time // when another sender took the output from it
	// Multi-room group from SETUP (groupUUID, groupContainsGroupLeader).
	groupUUID   string
	groupLeader bool
	// AirPlay 1 remote control (DACP), from the DACP-ID and Active-Remote
	// request headers.
	dacpID, activeRemote string
}

// advertisedState is what the TXT records currently say about sessions.
type advertisedState struct {
	gid    string
	leader bool
	active bool
}

// publishGroup makes the TXT records follow the sender that owns the output,
// as shairport-sync does for its principal connection: its group UUID and
// gcgl, and ReceiverSessionIsActive. Called with r.mu held.
func (r *Rstp) publishGroup() {
	var state advertisedState
	if r.active != nil && r.sessions[r.active] {
		info := r.sender(r.active)
		state = advertisedState{gid: info.groupUUID, leader: info.groupLeader, active: true}
	}
	if r.advertised != nil && *r.advertised == state {
		return
	}
	r.advertised = &state
	if homekit.Device == nil {
		return
	}
	homekit.Device.SetGroup(state.gid, state.leader)
	homekit.Device.SetSessionActive(state.active)
	if r.advertise != nil {
		r.advertise()
	}
}

// SetAdvertise sets the function that publishes changed TXT records.
func (r *Rstp) SetAdvertise(publish func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.advertise = publish
}

// A sender that was just asked to pause may send a last progress update
// or flush in reaction; that is not the user pressing play.
const resumeGrace = 2 * time.Second

// resumeSignal reports whether a request from a waiting sender means its
// user started playing. Senders always set up a stream or set a rate to
// play, except a paused realtime sender whose stream was not torn down yet:
// it keeps sending packets while paused, so pressing play only sends a
// progress update (sent on play, resume and track change) or a FLUSH (play
// or seek).
func (r *Rstp) resumeSignal(conn *rtsp.Conn, req *rtsp.Request) bool {
	if r.foreground(conn) || !r.sessions[conn] || r.classic[conn] != nil {
		return false // AirPlay 1 sends FLUSH to pause
	}
	switch req.Method {
	case "FLUSH":
	case "SET_PARAMETER":
		if !strings.EqualFold(strings.TrimSpace(headerValue(req, "Content-Type")), "text/parameters") ||
			!bytes.Contains(req.Body, []byte("progress:")) {
			return false
		}
	default:
		return false
	}
	if time.Since(r.sender(conn).pausedAt) < resumeGrace {
		return false
	}
	for _, owner := range r.owners {
		if owner == conn {
			return true // it has a stream that can play
		}
	}
	return false
}

func (r *Rstp) sender(conn *rtsp.Conn) *senderInfo {
	if conn == nil {
		return &senderInfo{}
	}
	if r.senders == nil {
		r.senders = make(map[*rtsp.Conn]*senderInfo)
	}
	info := r.senders[conn]
	if info == nil {
		info = &senderInfo{}
		r.senders[conn] = info
	}
	return info
}

func remoteHost(conn *rtsp.Conn) string {
	if conn == nil || conn.NetConn() == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.NetConn().RemoteAddr().String())
	if err != nil {
		return ""
	}
	return host
}

// foreground reports whether requests from conn may change the output and
// what the window shows.
func (r *Rstp) foreground(conn *rtsp.Conn) bool {
	return r.active == nil || r.active == conn
}

// admit is called when conn sets up a session. It becomes the active sender
// when no other sender is connected; otherwise it waits until it plays.
func (r *Rstp) admit(conn *rtsp.Conn) {
	// The same device connecting again replaces its old connection.
	if host := remoteHost(conn); host != "" {
		for other := range r.sessions {
			if other != conn && remoteHost(other) == host {
				r.drop(other, "replaced by a new connection from the same sender")
			}
		}
	}
	if r.active == nil || r.active == conn || !r.sessions[r.active] {
		r.activate(conn, false)
	}
}

// drop ends another connection's session and closes it.
func (r *Rstp) drop(conn *rtsp.Conn, reason string) {
	for path, owner := range r.owners {
		if owner == conn {
			r.dropStream(path)
		}
	}
	if r.sessions[conn] {
		delete(r.sessions, conn)
		log.Printf("SESSION ended (%s)", reason)
	}
	delete(r.senders, conn)
	r.updateTimingPeers()
	if r.active == conn {
		r.active = nil
	}
	conn.Close()
}

// updateTimingPeers passes every connected sender's addresses to PTP.
func (r *Rstp) updateTimingPeers() {
	if r.TimingPeers == nil {
		return
	}
	var ips []net.IP
	add := func(address string) {
		if ip := net.ParseIP(address); ip != nil && !containsIP(ips, ip) {
			ips = append(ips, ip)
		}
	}
	for conn := range r.sessions {
		add(remoteHost(conn))
		for _, peer := range r.sender(conn).peers {
			add(peer)
		}
	}
	r.TimingPeers(ips)
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, item := range list {
		if item.Equal(ip) {
			return true
		}
	}
	return false
}

// resumeFromAudio is called by a waiting realtime stream whose packets
// went from silence (paused) to audio: its user pressed play again.
func (r *Rstp) resumeFromAudio(conn *rtsp.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.sessions[conn] || r.foreground(conn) {
		return
	}
	log.Printf("SESSION waiting sender %q is playing again (its audio resumed)", r.sender(conn).name)
	r.startPlaying(conn)
	r.publishGroup()
}

// startPlaying is called when conn starts or resumes playback. If another
// sender owns the output, that sender is paused and conn takes over.
func (r *Rstp) startPlaying(conn *rtsp.Conn) {
	if r.active == conn {
		return
	}
	if previous := r.active; previous != nil {
		for path, owner := range r.owners {
			if owner == previous {
				r.streams[path].Suspend()
			}
		}
		r.sender(previous).pausedAt = time.Now()
		name := r.sender(previous).name
		log.Printf("SESSION paused sender=%q (another sender started playing)", name)
		// AirPlay 2 senders are asked over their event channel, AirPlay 1
		// senders over DACP when they named a remote control. Not while
		// holding r.mu: the request may wait for the network.
		if r.classic[previous] == nil || r.sender(previous).dacpID != "" {
			host := remoteHost(previous)
			go func() {
				if err := event.SendMediaRemoteCommandTo(host, event.CommandPause); err != nil {
					log.Printf("SESSION could not ask %q to pause: %v", name, err)
				}
			}()
		}
	}
	r.activate(conn, true)
}

// activate gives the output to conn. With announce, the window is told
// which sender is now shown, with that sender's volume and track.
func (r *Rstp) activate(conn *rtsp.Conn, announce bool) {
	r.active = conn
	for path, owner := range r.owners {
		if owner == conn {
			r.streams[path].Resume()
		}
	}
	info := r.sender(conn)
	host := remoteHost(conn)
	if r.classic[conn] != nil {
		// An AirPlay 1 sender's clock arrives over NTP; ignore PTP.
		if r.ClockSources != nil {
			r.ClockSources([]net.IP{})
		}
	} else if host != "" {
		r.setClockSources(append(append([]string(nil), info.peers...), host))
	}
	event.SetActiveSender(host)
	if !announce {
		return
	}
	resetPlaybackState()
	log.Printf("SESSION active sender=%q", info.name)
	if info.hasVolume {
		r.applyVolume(info.volume)
	}
	if info.track != nil {
		r.onTrackInfo(info.track)
	}
	if info.hasArt {
		r.onArtwork(info.artType, info.art)
	}
}

// latestSession returns the most recently started session, or nil.
func (r *Rstp) latestSession() *rtsp.Conn {
	var latest *rtsp.Conn
	var order uint64
	for conn := range r.sessions {
		if o := r.sender(conn).order; latest == nil || o > order {
			latest, order = conn, o
		}
	}
	return latest
}

// remember keeps the volume and now-playing information a sender sets.
func (r *Rstp) remember(conn *rtsp.Conn, req *rtsp.Request) {
	info := r.sender(conn)
	contentType := strings.ToLower(strings.TrimSpace(headerValue(req, "Content-Type")))
	switch {
	case strings.Contains(contentType, "dmap"):
		if _, ok := parseTrackInfo(req.Body); ok {
			info.track = append([]byte(nil), req.Body...)
		}
	case strings.HasPrefix(contentType, "image/"):
		info.artType, info.art, info.hasArt = contentType, append([]byte(nil), req.Body...), true
	case contentType == "text/parameters":
		scanner := bufio.NewScanner(bytes.NewReader(req.Body))
		for scanner.Scan() {
			var vol float64
			if c, err := fmt.Sscanf(scanner.Text(), "volume: %f", &vol); c == 1 && err == nil {
				info.volume, info.hasVolume = vol, true
			}
		}
	}
}

func (r *Rstp) applyVolume(vol float64) {
	config.Config.Volume = vol
	log.Printf("AUDIO volume %.1f dB", vol)
	r.player.ControlChannel <- globals.ControlMessage{MType: globals.VOLUME, Paramf: vol}
}
