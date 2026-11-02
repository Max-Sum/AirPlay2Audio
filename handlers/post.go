package handlers

import "github.com/Max-Sum/AirPlay2Audio/rtsp"

func (r *Rstp) OnPostWeb(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {

	switch req.Path {
	case "pair-add", "pair-remove", "pair-list":
		// Managing pairings is for a controller that proved its stored key
		// with pair-verify, not for anyone on the network (or a transient
		// pairing, which only shows the sender knows the public PIN).
		if !r.verified[conn] {
			return &rtsp.Response{StatusCode: rtsp.StatusConnectionAuthorizationRequired}, nil
		}
	}
	switch req.Path {
	case "pair-pin-start":
		// Sent before pair-setup when the receiver advertises pw=true. The
		// sender then asks for the password and uses it as the PIN.
		return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
	case "pair-setup":
		return r.OnPairSetup(conn, req)
	case "pair-verify":
		return r.OnPairVerify(conn, req)
	case "pair-add":
		return r.OnPairAdd(conn, req)
	case "pair-remove":
		return r.OnPairRemove(conn, req)
	case "pair-list":
		return r.OnPairList(conn, req)
	case "configure":
		return r.OnPairConfigure(req)
	case "fp-setup":
		return r.OnFpSetup(req)
	case "command":
		return r.OnCommand(req)
	case "audioMode":
		return r.OnAudioMode(req)
	case "feedback":
		return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
	}
	return &rtsp.Response{StatusCode: rtsp.StatusNotImplemented}, nil
}
