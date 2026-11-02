package handlers

import (
	"testing"

	"howett.net/plist"
)

// iOS names the anchor's PTP clock networkTimeTimelineID (as read by
// shairport-sync); it must reach AnchorClock.
func TestSetRateAnchorTimeReadsTimelineID(t *testing.T) {
	body, err := plist.Marshal(map[string]interface{}{
		"rate": uint64(1), "rtpTime": uint64(1000),
		"networkTimeSecs": uint64(10), "networkTimeFrac": uint64(0),
		"networkTimeTimelineID": uint64(0x1122334455667788),
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	var content setRateAnchorTime
	if _, err := plist.Unmarshal(body, &content); err != nil {
		t.Fatal(err)
	}
	if content.ClockIdentity != 0x1122334455667788 {
		t.Fatalf("clock identity = %#x", content.ClockIdentity)
	}
}
