package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Max-Sum/AirPlay2Audio/rtsp"
)

func captureMedia(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	previous := MediaOutput
	MediaOutput = &out
	resetPlaybackState()
	t.Cleanup(func() { MediaOutput = previous; resetPlaybackState() })
	return &out
}

func TestTrackInfoIsSentAsMediaLine(t *testing.T) {
	out := captureMedia(t)
	duration := make([]byte, 4)
	binary.BigEndian.PutUint32(duration, 215000)
	inner := append(dmapItem("minm", []byte("晴天")), dmapItem("asar", []byte("周杰伦"))...)
	inner = append(inner, dmapItem("astm", duration)...)
	handler := &Rstp{}
	response, err := handler.OnSetParameterWeb(&rtsp.Request{Header: rtsp.Header{"Content-Type": {"application/x-dmap-tagged"}}, Body: dmapItem("mlit", inner)})
	if err != nil || response.StatusCode != rtsp.StatusOK {
		t.Fatalf("SET_PARAMETER: %v %+v", err, response)
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	want := "@media meta " + b64("晴天") + " " + b64("周杰伦") + " - 215000\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestProgressReportsPositionAndPlaying(t *testing.T) {
	out := captureMedia(t)
	handler := &Rstp{}
	// Start near the 32-bit limit so the current and end stamps wrap.
	body := "progress: 4294923196/44100/441000\r\n"
	if _, err := handler.OnSetParameterWeb(&rtsp.Request{Header: rtsp.Header{"Content-Type": {"text/parameters"}}, Body: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "@media progress 2000 11000\n@media state playing\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	setPlaybackState(true) // unchanged state is not repeated
	setPlaybackState(false)
	if !strings.HasSuffix(out.String(), "playing\n@media state paused\n") {
		t.Fatalf("state lines %q", out.String())
	}
}

func TestArtworkIsSavedAndCleared(t *testing.T) {
	out := captureMedia(t)
	previous := ArtworkPath
	ArtworkPath = filepath.Join(t.TempDir(), "art")
	t.Cleanup(func() { ArtworkPath = previous })
	handler := &Rstp{}
	image := []byte{0xff, 0xd8, 0xff, 0xe0}
	if _, err := handler.OnSetParameterWeb(&rtsp.Request{Header: rtsp.Header{"Content-Type": {"image/jpeg"}}, Body: image}); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(ArtworkPath)
	if err != nil || !bytes.Equal(saved, image) {
		t.Fatalf("saved artwork %v %v", saved, err)
	}
	if _, err := handler.OnSetParameterWeb(&rtsp.Request{Header: rtsp.Header{"Content-Type": {"image/none"}}}); err != nil {
		t.Fatal(err)
	}
	want := "@media art " + base64.StdEncoding.EncodeToString([]byte(ArtworkPath)) + "\n@media art -\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}
