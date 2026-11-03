package handlers

import (
	"github.com/Max-Sum/AirPlay2Audio/audio"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/homekit"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
	"howett.net/plist"
	"log"
	"net"
	"strings"
)

type setupStreamRequest struct {
	Type            uint8    `plist:"type"`
	SharedKey       [32]byte `plist:"shk"`
	Spf             uint32   `plist:"spf"`
	CompressionType uint32   `plist:"ct"`
	AudioFormat     uint64   `plist:"audioFormat"` // 5.1 and 7.1 use bits above 32
	SampleRate      uint32   `plist:"sr"`
}

type setupRtsp struct {
	Streams        []setupStreamRequest `plist:"streams"`
	Name           string               `plist:"name"`
	TimingProtocol string               `plist:"timingProtocol"`
	RemoteOnly     bool                 `plist:"isRemoteControlOnly"`
	// Multi-room: the sender's group, published in the gid and gcgl TXT
	// records while this sender owns the output.
	GroupUUID   string `plist:"groupUUID"`
	GroupLeader bool   `plist:"groupContainsGroupLeader"`
}

// Stream types in SETUP.
const (
	streamRealtime      = 96
	streamBuffered      = 103
	streamRemoteControl = 130
)

type remoteControlStream struct {
	StreamID uint64 `plist:"streamID"`
	Type     uint64 `plist:"type"`
}

type remoteControlResponse struct {
	Streams []remoteControlStream `plist:"streams"`
}

// pickStream returns the stream a SETUP is about. Like shairport-sync, only
// one stream is set up; the first audio stream wins over the others.
func pickStream(streams []setupStreamRequest) (setupStreamRequest, bool) {
	for _, stream := range streams {
		if stream.Type == streamRealtime || stream.Type == streamBuffered {
			return stream, true
		}
	}
	for _, stream := range streams {
		if stream.Type == streamRemoteControl {
			return stream, true
		}
	}
	return setupStreamRequest{}, false
}

func binaryPlistResponse(value interface{}) (*rtsp.Response, error) {
	body, err := plist.Marshal(value, plist.BinaryFormat)
	if err != nil {
		return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK, Header: rtsp.Header{
		"Content-Type": rtsp.HeaderValue{"application/x-apple-binary-plist"},
	}, Body: body}, nil
}

type peerInfos struct {
	Addresses                 []string `plist:"Addresses"`
	Id                        string   `plist:"ID"`
	ClockPortMatchingOverride bool     `plist:"SupportsClockPortMatchingOverride"`
}

type setupEventResponse struct {
	EventPort  uint16    `plist:"eventPort"`
	PeerInfo   peerInfos `plist:"timingPeerInfo"`
	TimingPort uint16    `plist:"timingPort"`
}

type setupStream struct {
	BufferSize  uint32 `plist:"audioBufferSize"`
	AudioFormat uint64 `plist:"audioFormat"`
	DataPort    uint16 `plist:"dataPort"`
	ControlPort uint16 `plist:"controlPort"`
	TypeStream  uint8  `plist:"type"`
}

type setupSteamsResponse struct {
	Streams []setupStream `plist:"streams"`
}

func (r *Rstp) OnSetupWeb(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {

	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		var content setupRtsp
		if _, err := plist.Unmarshal(req.Body, &content); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
		}
		if content.Streams != nil {
			stream, ok := pickStream(content.Streams)
			if !ok {
				for _, stream := range content.Streams {
					log.Printf("Unsupported stream: type=%d format=%#x codec=%d samples=%d", stream.Type, stream.AudioFormat, stream.CompressionType, stream.Spf)
				}
				return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
			}
			if len(content.Streams) > 1 {
				log.Printf("SETUP with %d streams; setting up type %d only", len(content.Streams), stream.Type)
			}
			if stream.Type == streamRemoteControl {
				// A remote control channel carries no audio. Acknowledge it as
				// shairport-sync does, without a data port.
				log.Printf("CONTROL remote control stream set up by %s", remoteHost(conn))
				return binaryPlistResponse(remoteControlResponse{Streams: []remoteControlStream{{StreamID: 1, Type: streamRemoteControl}}})
			}
			if s, found := r.streams[req.Path]; found {
				var port, controlPort int
				var err error
				switch stream.Type {
				case streamBuffered:
					// The format named here is only the initial one: each
					// packet's SSRC names its encoding.
					if !audio.SupportedBufferedFormat(stream.AudioFormat) {
						log.Printf("AUDIO buffered format %#x is not one this receiver decodes; packets will be decoded by their SSRC", stream.AudioFormat)
					}
					log.Printf("AUDIO buffered setup: format=%#x samples=%d", stream.AudioFormat, stream.Spf)
					port, err = s.Setup(stream.SharedKey[:], stream.AudioFormat)
				case streamRealtime:
					spf := stream.Spf
					if spf == 0 {
						spf = 352
					}
					format, ok := audio.RealtimeFormatFor(stream.AudioFormat, stream.SampleRate)
					if !ok || spf > 4096 {
						log.Printf("Unsupported realtime stream: format=%#x rate=%d samples=%d (ALAC stereo at 44100 or 48000 Hz, 16 or 24-bit only)", stream.AudioFormat, stream.SampleRate, stream.Spf)
						return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
					}
					log.Printf("AUDIO realtime ALAC setup: format=%#x %d Hz %d-bit samples=%d", stream.AudioFormat, format.Rate, format.Bits, spf)
					// A realtime sender sets up its stream when it starts or
					// resumes playing.
					r.startPlaying(conn)
					port, controlPort, err = s.SetupRealtime(stream.SharedKey[:], spf, format)
					if err == nil {
						log.Printf("AUDIO realtime ports given to %s: data=%d control=%d", remoteHost(conn), port, controlPort)
					}
				}
				if err != nil {
					log.Printf("AUDIO setup error: %v", err)
					return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
				}
				if r.foreground(conn) {
					setPlaybackState(true)
				}
				setupStreamsResponse := setupSteamsResponse{
					Streams: []setupStream{{
						BufferSize:  globals.BufferSize,
						AudioFormat: stream.AudioFormat,
						DataPort:    uint16(port),
						ControlPort: uint16(controlPort),
						TypeStream:  stream.Type,
					}},
				}
				if body, err := plist.Marshal(setupStreamsResponse, plist.AutomaticFormat); err == nil {
					return &rtsp.Response{StatusCode: rtsp.StatusOK, Header: rtsp.Header{
						"Content-Type": rtsp.HeaderValue{"application/x-apple-binary-plist"},
					}, Body: body}, nil
				}
			}
		} else if content.RemoteOnly {
			// A sender that only wants to control playback (for example an
			// iPhone controlling a group from Control Center). It must not
			// become a playing session or take the output.
			log.Printf("CONTROL remote-control-only session from %s", remoteHost(conn))
			return binaryPlistResponse(setupEventResponse{PeerInfo: peerInfos{
				Addresses: homekit.Server.Ips,
				Id:        homekit.Device.Pi.String(),
			}, EventPort: uint16(r.EventPort)})
		} else {
			if content.TimingProtocol != "" && content.TimingProtocol != "PTP" {
				log.Printf("SETUP timing protocol %q; AirPlay 2 playback here is timed by PTP", content.TimingProtocol)
			}
			if r.interrupts(conn) {
				return busy(), nil
			}
			stream, err := audio.NewServer(r.player)
			if err != nil {
				return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, err
			}
			r.admit(conn)
			if r.active != conn {
				stream.Standby()
			}
			stream.OnResume = func() { r.resumeFromAudio(conn) }
			if old, found := r.streams[req.Path]; found {
				old.Teardown()
			}
			r.streams[req.Path] = stream
			if r.owners == nil {
				r.owners = make(map[string]*rtsp.Conn)
			}
			r.owners[req.Path] = conn
			r.sessionStarted(conn, content.Name)
			info := r.sender(conn)
			info.groupUUID, info.groupLeader = content.GroupUUID, content.GroupLeader
			if content.GroupUUID != "" {
				log.Printf("SESSION group %s leader=%t", content.GroupUUID, content.GroupLeader)
			}
			setupEventResponse := setupEventResponse{PeerInfo: peerInfos{
				Addresses:                 homekit.Server.Ips,
				Id:                        homekit.Device.Pi.String(),
				ClockPortMatchingOverride: false,
			}, EventPort: uint16(r.EventPort)}
			if body, err := plist.Marshal(setupEventResponse, plist.AutomaticFormat); err == nil {
				return &rtsp.Response{StatusCode: rtsp.StatusOK, Header: rtsp.Header{
					"Content-Type": rtsp.HeaderValue{"application/x-apple-binary-plist"},
				}, Body: body}, nil
			}
		}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
}

func (r *Rstp) OnSetPeerWeb(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {

	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		// The peer list names every device in the group, including the
		// one which may be the PTP clock master (for example a HomePod).
		var peers []string
		if _, err := plist.Unmarshal(req.Body, &peers); err == nil {
			r.applyPeers(conn, peers)
		}
		var content map[string]interface{}
		if _, err := plist.Unmarshal(req.Body, &content); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
		}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

// applyPeers records the group's timing peers and, for the sender that owns
// the output, lets PTP follow any of their clocks.
func (r *Rstp) applyPeers(conn *rtsp.Conn, peers []string) {
	r.sender(conn).peers = append([]string(nil), peers...)
	r.updateTimingPeers()
	if conn == r.active {
		if host := remoteHost(conn); host != "" {
			peers = append(peers, host)
		}
		r.setClockSources(peers)
	}
}

// extendedPeer is one entry of SETPEERSX: a group member with all of its
// addresses and its PTP clock.
type extendedPeer struct {
	Addresses []string `plist:"Addresses"`
	ID        string   `plist:"ID"`
	ClockID   uint64   `plist:"ClockID"`
}

// OnSetPeersX handles SETPEERSX, the extended SETPEERS that names each peer
// with its addresses rather than one address. Its addresses are used as
// SETPEERS addresses. (shairport-sync acknowledges it without using it.)
func (r *Rstp) OnSetPeersX(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	var entries []extendedPeer
	if _, err := plist.Unmarshal(req.Body, &entries); err != nil {
		// Not every sender sends the same layout; accept what cannot be read.
		log.Printf("SETPEERSX body not understood (%d bytes); acknowledged", len(req.Body))
		return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
	}
	var peers []string
	for _, entry := range entries {
		for _, address := range entry.Addresses {
			if net.ParseIP(address) != nil {
				peers = append(peers, address)
			}
		}
	}
	log.Printf("SETPEERSX %d peers, %d addresses", len(entries), len(peers))
	r.applyPeers(conn, peers)
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}
