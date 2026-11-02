package ptp

import (
	"net"
	"testing"
)

func TestClockIdentityFromMAC(t *testing.T) {
	mac := net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}
	if got := ClockIdentityFromMAC(mac); got != 0x001122fffe334455 {
		t.Fatalf("identity = %016x", got)
	}
	other := net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x56}
	if ClockIdentityFromMAC(mac) == ClockIdentityFromMAC(other) {
		t.Fatal("different addresses gave the same identity")
	}
	request, err := NewDelayRequest(0x001122fffe334455, 7).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	// The clock identity is bytes 20..28 of the PTP header.
	for i, b := range []byte{0x00, 0x11, 0x22, 0xff, 0xfe, 0x33, 0x44, 0x55} {
		if request[20+i] != b {
			t.Fatalf("Delay_Req identity byte %d = %#x", i, request[20+i])
		}
	}
}
