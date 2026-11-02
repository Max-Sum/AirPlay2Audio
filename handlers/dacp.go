package handlers

import (
	"log"

	"github.com/Max-Sum/AirPlay2Audio/dacp"
	"github.com/Max-Sum/AirPlay2Audio/event"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
)

// noteRemote keeps the DACP remote control a sender names in its request
// headers. AirPlay 1 senders can only be controlled this way; for AirPlay 2
// senders the event channel is used first and DACP is a fallback.
// Called with r.mu held.
func (r *Rstp) noteRemote(conn *rtsp.Conn, req *rtsp.Request) {
	id, active := headerValue(req, "DACP-ID"), headerValue(req, "Active-Remote")
	if id == "" || active == "" || !isHexIdentifier(id) {
		return
	}
	info := r.sender(conn)
	if info.dacpID == id && info.activeRemote == active {
		return
	}
	host := remoteHost(conn)
	if host == "" {
		return
	}
	info.dacpID, info.activeRemote = id, active
	event.RegisterRemote(host, dacp.New(host, id, active))
	log.Printf("CONTROL DACP remote control available for %s", host)
}

// forgetRemote removes the remote control of a closed connection.
func (r *Rstp) forgetRemote(conn *rtsp.Conn) {
	if info := r.senders[conn]; info != nil && info.dacpID != "" {
		event.UnregisterRemote(remoteHost(conn), info.dacpID)
	}
}

func isHexIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
