package handlers

import (
	"bytes"
	"encoding/binary"
	"log"
	"reflect"
	"strings"
	"testing"

	"goplay2/globals"
	"goplay2/rtsp"
	"howett.net/plist"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	previousOut, previousFlags, previousTrace := log.Writer(), log.Flags(), globals.Trace
	log.SetOutput(&out)
	log.SetFlags(0)
	globals.Trace = true
	diagMu.Lock()
	diagLast = map[string]string{}
	diagMu.Unlock()
	t.Cleanup(func() {
		log.SetOutput(previousOut)
		log.SetFlags(previousFlags)
		globals.Trace = previousTrace
	})
	return &out
}

func dmapItem(tag string, value []byte) []byte {
	out := []byte(tag)
	out = binary.BigEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}

func TestDmapTagsListsNamesInsideContainer(t *testing.T) {
	inner := append(dmapItem("minm", []byte("Song")), dmapItem("asar", []byte("Artist"))...)
	body := dmapItem("mlit", inner)
	if got, want := dmapTags(body), []string{"asar", "minm", "mlit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags %v, want %v", got, want)
	}
	// A truncated item stops parsing instead of reading past the body.
	if got := dmapTags(body[:len(body)-2]); !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("truncated body tags %v", got)
	}
}

func TestParameterDiagnosticsOmitValues(t *testing.T) {
	out := captureLog(t)
	body := dmapItem("mlit", dmapItem("minm", []byte("Secret Title")))
	diagnoseParameter(&rtsp.Request{Header: rtsp.Header{"Content-Type": {"application/x-dmap-tagged"}}, Body: body})
	diagnoseParameter(&rtsp.Request{Header: rtsp.Header{"Content-Type": {"text/parameters"}}, Body: []byte("progress: 1/2/3\r\n")})
	diagnoseParameter(&rtsp.Request{Header: rtsp.Header{"Content-Type": {"text/parameters"}}, Body: []byte("progress: 4/5/6\r\n")})
	text := out.String()
	if strings.Contains(text, "Secret") || strings.Contains(text, "1/2/3") {
		t.Fatalf("diagnostics logged media values: %s", text)
	}
	if !strings.Contains(text, "dmap tags minm mlit") || strings.Count(text, "parameter text fields [progress]") != 1 {
		t.Fatalf("unexpected diagnostics: %s", text)
	}
}

func TestSupportedCommandDiagnostics(t *testing.T) {
	out := captureLog(t)
	var entries [][]byte
	for _, entry := range []map[string]interface{}{
		{"kCommandInfoCommandKey": 4, "kCommandInfoEnabledKey": true},
		{"kCommandInfoCommandKey": 5, "kCommandInfoEnabledKey": false},
		{"kCommandInfoCommandKey": 0},
	} {
		raw, err := plist.Marshal(entry, plist.BinaryFormat)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, raw)
	}
	diagnoseSupportedCommands(entries)
	text := out.String()
	if !strings.Contains(text, "enabled=[0 4] disabled=[5]") || !strings.Contains(text, "[kCommandInfoCommandKey kCommandInfoEnabledKey]") {
		t.Fatalf("unexpected diagnostics: %s", text)
	}
}

func TestRemoteControlDiagnosticsHideToken(t *testing.T) {
	out := captureLog(t)
	diagnoseRemoteControl(nil, &rtsp.Request{Method: "RECORD", Header: rtsp.Header{"Active-Remote": {"123456789"}}})
	text := out.String()
	if strings.Contains(text, "123456789") || !strings.Contains(text, `dacp-id="" active-remote=true`) {
		t.Fatalf("unexpected diagnostics: %s", text)
	}
}
