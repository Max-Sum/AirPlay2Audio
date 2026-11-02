package handlers

import (
	"goplay2/audio"
	"goplay2/config"
	"goplay2/globals"
	"goplay2/rtsp"
	"testing"
)

// Exercise the same handler used by a sender during playback, including
// multiple live changes and the AirPlay mute sentinel.
func TestSenderVolumeUpdatesReachPlayer(t *testing.T) {
	previous := config.Config.Volume
	t.Cleanup(func() { config.Config.Volume = previous })
	player := &audio.Player{ControlChannel: make(chan globals.ControlMessage, 3)}
	handler := &Rstp{player: player}
	for _, tc := range []struct {
		body string
		want float64
	}{
		{"volume: 0.000000\r\n", 0},
		{"volume: -20.000000\r\n", -20},
		{"volume: -144.000000\r\n", -144},
	} {
		response, err := handler.OnSetParameterWeb(&rtsp.Request{Header: rtsp.Header{"Content-Type": {"text/parameters"}}, Body: []byte(tc.body)})
		if err != nil || response.StatusCode != rtsp.StatusOK {
			t.Fatalf("SET_PARAMETER: %v %+v", err, response)
		}
		msg := <-player.ControlChannel
		if msg.MType != globals.VOLUME || msg.Paramf != tc.want || config.Config.Volume != tc.want {
			t.Fatalf("volume update %#v, stored %v; want %v", msg, config.Config.Volume, tc.want)
		}
	}
}
