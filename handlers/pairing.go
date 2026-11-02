package handlers

import (
	"bytes"
	"github.com/brutella/hc/crypto"
	"github.com/brutella/hc/hap"
	"github.com/brutella/hc/hap/pair"
	"github.com/brutella/hc/util"
	"github.com/Max-Sum/AirPlay2Audio/event"
	"github.com/Max-Sum/AirPlay2Audio/homekit"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
	"howett.net/plist"
	"log"
	"strings"
)

func (r *Rstp) OnPairSetup(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {
	var err error
	var in util.Container
	var out util.Container

	key := conn.NetConn().RemoteAddr().String()
	session := homekit.Server.Context.Get(key).(hap.Session)

	if in, err := util.NewTLV8ContainerFromReader(bytes.NewReader(req.Body)); err == nil {
		step := in.GetByte(pair.TagSequence)
		if isTransientStart(in) {
			log.Print("PAIR transient setup step=1")
			srpSession, err := pair.NewSetupServerSession(homekit.Server.Name, r.auth.pin())
			if err != nil {
				return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, err
			}
			r.transient[key] = srpSession
			out := util.NewTLV8Container()
			out.SetByte(pair.TagSequence, 2)
			out.SetBytes(pair.TagSalt, srpSession.Salt)
			out.SetBytes(pair.TagPublicKey, srpSession.PublicKey)
			return &rtsp.Response{StatusCode: rtsp.StatusOK, Body: out.BytesBuffer().Bytes()}, nil
		}
		if srpSession, ok := r.transient[key]; ok && step == 3 {
			delete(r.transient, key)
			log.Print("PAIR transient setup step=3")
			if err := srpSession.SetupPrivateKeyFromClientPublicKey(in.GetBytes(pair.TagPublicKey)); err != nil {
				return &rtsp.Response{StatusCode: rtsp.StatusOK, Body: transientError(4).BytesBuffer().Bytes()}, nil
			}
			proof, err := srpSession.ProofFromClientProof(in.GetBytes(pair.TagProof))
			if err != nil {
				if r.auth.password != "" {
					log.Print("AUTH wrong password")
				} else {
					log.Print("PAIR transient setup rejected: wrong proof")
				}
				return &rtsp.Response{StatusCode: rtsp.StatusOK, Body: transientError(4).BytesBuffer().Bytes()}, nil
			}
			// With a password, the sender used it as the PIN, so this
			// connection has proven it knows the password.
			if r.auth.password != "" {
				r.auth.allow(conn)
				log.Print("AUTH password accepted")
			}
			secret := append([]byte(nil), srpSession.PrivateKey...)
			cryptographer, err := newControlSession(secret)
			if err != nil {
				return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, err
			}
			out := util.NewTLV8Container()
			out.SetByte(pair.TagSequence, 4)
			out.SetBytes(pair.TagProof, proof)
			// Takes effect on the next read, so this response is still plain.
			session.SetCryptographer(cryptographer)
			event.RegisterPairing(conn.NetConn().RemoteAddr(), secret)
			log.Print("PAIR transient setup complete")
			return &rtsp.Response{StatusCode: rtsp.StatusOK, Body: out.BytesBuffer().Bytes()}, nil
		}
	}

	ctrl := session.PairSetupHandler()
	if ctrl == nil {

		if ctrl, err = pair.NewSetupServerController(homekit.Server.Device, homekit.Server.Database); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, err
		}
		session.SetPairSetupHandler(ctrl)
	}

	if in, err = util.NewTLV8ContainerFromReader(bytes.NewReader(req.Body)); err == nil {
		log.Printf("PAIR setup step=%d", in.GetByte(pair.TagSequence))
		out, err = ctrl.Handle(in)
	}

	/*var secSession crypto.Cryptographer

	// When key verification is done, switch to a secure session
	// based on the negotiated shared session key
	b := out.GetByte(pair.TagSequence)
	switch pair.VerifyStepType(b) {
	case pair.VerifyStepFinishResponse:
		if out.GetByte(pair.TagErrCode) != 0 { break }
		if secSession, err = crypto.NewSecureSessionFromSharedKey(ctrl.SharedKey()); err == nil {
			session.SetCryptographer(secSession)
		} else {
			return &base.Response{StatusCode: base.StatusInternalServerError}, err
		}
	}*/

	if err != nil {
		return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, err
	}
	body := out.BytesBuffer().Bytes()

	return &rtsp.Response{StatusCode: rtsp.StatusOK, Body: body}, nil
}

func (r *Rstp) OnPairVerify(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {

	key := conn.NetConn().RemoteAddr().String()
	session := homekit.Server.Context.Get(key).(hap.Session)

	var err error
	var in util.Container
	var out util.Container
	var secSession crypto.Cryptographer

	if in, err = util.NewTLV8ContainerFromReader(bytes.NewReader(req.Body)); err != nil {
		return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
	}
	step := in.GetByte(pair.TagSequence)
	log.Printf("PAIR verify step=%d", step)
	if step == byte(pair.VerifyStepFinishRequest) && len(in.GetBytes(pair.TagEncryptedData)) < 16 {
		return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, nil
	}
	ctlr := session.PairVerifyHandler()
	// A client may retry M1 on the same connection after a failed attempt.
	if ctlr == nil || step == byte(pair.VerifyStepStartRequest) {
		delete(r.verified, conn) // a restarted verify must succeed again
		ctlr = pair.NewVerifyServerController(homekit.Server.Database, homekit.Server.Context)
		session.SetPairVerifyHandler(ctlr)
	}
	out, err = ctlr.Handle(in)

	if err != nil {
		return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, err
	}
	body := out.BytesBuffer().Bytes()

	// When key verification is done, switch to a secure session
	// based on the negotiated shared session key
	b := out.GetByte(pair.TagSequence)
	switch pair.VerifyStepType(b) {
	case pair.VerifyStepFinishResponse:
		if out.GetByte(pair.TagErrCode) != 0 {
			break
		}
		if secSession, err = crypto.NewSecureSessionFromSharedKey(ctlr.SharedKey()); err == nil {
			session.SetCryptographer(secSession)
			if r.verified == nil {
				r.verified = make(map[*rtsp.Conn]bool)
			}
			r.verified[conn] = true
			sharedKey := ctlr.SharedKey()
			event.RegisterPairing(conn.NetConn().RemoteAddr(), sharedKey[:])
		} else {
			return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, err
		}
	}

	return &rtsp.Response{StatusCode: rtsp.StatusOK, Body: body}, nil
}

func (r *Rstp) OnPairAdd(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {

	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		if container, err := util.NewTLV8ContainerFromReader(bytes.NewReader(req.Body)); err == nil {
			outputContainer, err := r.pairing.Handle(container)
			if err != nil {
				return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
			}
			return &rtsp.Response{StatusCode: rtsp.StatusOK, Body: outputContainer.BytesBuffer().Bytes()}, nil
		}

	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

func (r *Rstp) OnPairRemove(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {

	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		if container, err := util.NewTLV8ContainerFromReader(bytes.NewReader(req.Body)); err == nil {
			outputContainer, err := r.pairing.Handle(container)
			if err != nil {
				return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
			}
			return &rtsp.Response{StatusCode: rtsp.StatusOK, Body: outputContainer.BytesBuffer().Bytes()}, nil
		}

	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

func (r *Rstp) OnPairList(conn *rtsp.Conn, req *rtsp.Request) (*rtsp.Response, error) {

	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		if container, err := util.NewTLV8ContainerFromReader(bytes.NewReader(req.Body)); err == nil {
			outputContainer, err := r.pairing.Handle(container)
			if err != nil {
				return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
			}
			return &rtsp.Response{StatusCode: rtsp.StatusOK, Body: outputContainer.BytesBuffer().Bytes()}, nil
		}
	}
	return &rtsp.Response{StatusCode: rtsp.StatusOK}, nil
}

func (r *Rstp) OnPairConfigure(req *rtsp.Request) (*rtsp.Response, error) {

	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		var content map[string]interface{}
		if _, err := plist.Unmarshal(req.Body, &content); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
		}
	}

	config := homekit.Configuration{
		DeviceName:           homekit.Server.Name,
		AccessControlEnabled: true,
		AccessControlLevel:   0,
		Identifier:           homekit.Device.Pi.String(),
		PublicKey:            homekit.Server.Device.PublicKey(),
	}

	if body, err := plist.Marshal(config, plist.AutomaticFormat); err == nil {
		return &rtsp.Response{StatusCode: rtsp.StatusOK, Header: rtsp.Header{
			"Content-Type": rtsp.HeaderValue{"application/x-apple-binary-plist"},
		}, Body: body}, nil
	}
	return &rtsp.Response{StatusCode: rtsp.StatusInternalServerError}, nil
}
