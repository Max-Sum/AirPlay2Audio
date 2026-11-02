package event

import (
	"strings"
	"testing"

	"howett.net/plist"
)

func TestDescribeEventListsFieldNamesOnly(t *testing.T) {
	body, err := plist.Marshal(map[string]interface{}{
		"type":   "updateInfo",
		"params": map[string]interface{}{"title": "Secret Title"},
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	got := describeEvent([]string{"POST", "/command", "RTSP/1.0"}, "application/x-apple-binary-plist", body)
	if strings.Contains(got, "Secret") || strings.Contains(got, "updateInfo") {
		t.Fatalf("description includes values: %s", got)
	}
	if !strings.Contains(got, "POST /command RTSP/1.0") || !strings.Contains(got, "fields=[params type] params=[title]") {
		t.Fatalf("unexpected description: %s", got)
	}
}
