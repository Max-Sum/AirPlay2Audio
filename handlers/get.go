package handlers

import (
	"goplay2/audio"
	"goplay2/config"
	"goplay2/homekit"
	"goplay2/rtsp"
	"howett.net/plist"
	"strings"
)

type getInfoContent struct {
	Qualifier []string `plist:"qualifier"`
}

type audioLatenciesResponse struct {
	AudioType           string `plist:"audioType"`
	InputLatencyMicros  uint64 `plist:"inputLatencyMicros"`
	OutputLatencyMicros uint64 `plist:"outputLatencyMicros"`
	Type                uint64 `plist:"type"`
}

type getInfoResponse struct {
	AudioLatencies    []audioLatenciesResponse `plist:"audioLatencies"`
	DeviceId          string                   `plist:"deviceID"`
	Features          uint64                   `plist:"features"`
	Pi                string                   `plist:"pi"`
	Psi               string                   `plist:"psi"`
	ProtocolVersion   string                   `plist:"protocolVersion"`
	Sdk               string                   `plist:"sdk"`
	SourceVersion     string                   `plist:"sourceVersion"`
	StatusFlags       uint64                   `plist:"statusFlags"`
	InitialVolume     float64                  `plist:"initialVolume"`
	VolumeControlType uint64                   `plist:"volumeControlType"`
	Name              string                   `plist:"name"`
	SupportedFormats  map[string]uint64        `plist:"supportedFormats,omitempty"`
	Model             string                   `plist:"model"`
	PublicKey         []byte                   `plist:"pk"`
	Version           uint64                   `plist:"vv"`
	TxtAirPlay        []byte                   `plist:"txtAirPlay"`
}

func NewGetInfoResponse(deviceId string, features uint64, pi string,
	psi string, sourceVersion string) *getInfoResponse {

	latencies := [1]audioLatenciesResponse{{
		InputLatencyMicros:  0,
		OutputLatencyMicros: 400000,
		Type:                100,
	}}

	return &getInfoResponse{
		AudioLatencies:  latencies[:],
		DeviceId:        deviceId,
		Features:        features,
		Pi:              pi,
		Psi:             psi,
		ProtocolVersion: "1.1",
		Sdk:             "AirPlay;2.0.4",
		SourceVersion:   sourceVersion,
		StatusFlags:     0x4,
		InitialVolume:   config.Config.Volume,
		// 4 is the value reported by Sonos speakers; with it the system
		// volume (Control Center, hardware keys) drives this receiver on
		// recent iOS. iOS 18 ignores it for third-party receivers.
		VolumeControlType: 4,
		Name:              config.Config.DeviceName,
		SupportedFormats:  map[string]uint64{"audioStream": audio.RealtimeFormats, "bufferStream": audio.BufferedFormats},
	}
}

// InfoValue is the /info dictionary, also sent as updateInfo on the event
// channel.
func InfoValue() interface{} {
	info := NewGetInfoResponse(homekit.Device.Deviceid, homekit.Device.Features.ToUint64(),
		homekit.Device.Pi.String(), homekit.Device.Psi.String(), homekit.Device.Srcvers)
	info.StatusFlags = homekit.Device.StatusFlags()
	info.Model = homekit.Device.Model()
	info.PublicKey = homekit.Device.PublicKey()
	info.Version = 2
	info.TxtAirPlay = homekit.Device.TXTData()
	return *info
}

func (r *Rstp) OnGetInfo(req *rtsp.Request) (*rtsp.Response, error) {

	if contentType, found := req.Header["Content-Type"]; found && strings.EqualFold(contentType[0], "application/x-apple-binary-plist") {
		var content getInfoContent
		if _, err := plist.Unmarshal(req.Body, &content); err != nil {
			return &rtsp.Response{StatusCode: rtsp.StatusBadRequest}, err
		}
	}

	if body, err := plist.Marshal(InfoValue(), plist.AutomaticFormat); err == nil {
		return &rtsp.Response{StatusCode: rtsp.StatusOK, Header: rtsp.Header{
			"Content-Type": rtsp.HeaderValue{"application/x-apple-binary-plist"},
		}, Body: body}, nil
	}

	return &rtsp.Response{StatusCode: rtsp.StatusNotImplemented}, nil
}

func (r *Rstp) OnGetWeb(req *rtsp.Request) (*rtsp.Response, error) {

	switch req.Path {
	case "info":
		return r.OnGetInfo(req)
	}
	return &rtsp.Response{StatusCode: rtsp.StatusNotImplemented}, nil
}
