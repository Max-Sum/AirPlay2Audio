package rtsp

import (
	"bufio"
	"strings"
	"testing"
)

// iOS sends IPv6 URLs without brackets once it connects over IPv6.
func TestRequestWithBareIPv6URL(t *testing.T) {
	for raw, path := range map[string]string{
		"rtsp://2406:3003:2003:7dd6:42ed:35b1:dd4:dcf2/9506468210166026133": "9506468210166026133",
		"rtsp://fe80::1%en0/123/audio":                                      "123/audio",
		"rtsp://[fe80::1]/456":                                              "456",
		"rtsp://192.168.0.11/789":                                           "789",
	} {
		var req Request
		text := "SETUP " + raw + " RTSP/1.0\r\nCSeq: 1\r\n\r\n"
		if err := req.Read(bufio.NewReader(strings.NewReader(text))); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if req.Path != path {
			t.Fatalf("%s: path %q, want %q", raw, req.Path, path)
		}
	}
	var req Request
	if err := req.Read(bufio.NewReader(strings.NewReader("SETUP rtsp://not a host/1 RTSP/1.0\r\n\r\n"))); err == nil {
		t.Fatal("malformed URL accepted")
	}
}
