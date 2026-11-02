package handlers

import (
	"bytes"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/homekit"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
	"testing"

	"howett.net/plist"
)

func TestGetInfoReturnsAdvertisedTXTAndIdentity(t *testing.T) {
	previous := homekit.Device
	t.Cleanup(func() { homekit.Device = previous })
	homekit.Device = homekit.NewAccessory("02:00:00:00:00:01", "00000000-0000-0000-0000-000000000001", globals.NewFeatures())
	key := bytes.Repeat([]byte{0x42}, 32)
	homekit.Device.SetPublicKey(key)
	query, err := plist.Marshal(map[string]interface{}{"qualifier": []string{"txtAirPlay"}}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	req := &rtsp.Request{Header: rtsp.Header{"Content-Type": {"application/x-apple-binary-plist"}}, Body: query}
	response, err := (&Rstp{}).OnGetInfo(req)
	if err != nil || response.StatusCode != rtsp.StatusOK {
		t.Fatalf("info: %v, %+v", err, response)
	}
	var info struct {
		TXT     []byte `plist:"txtAirPlay"`
		Key     []byte `plist:"pk"`
		Version uint64 `plist:"vv"`
		Model   string `plist:"model"`
	}
	if _, err := plist.Unmarshal(response.Body, &info); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(info.TXT, homekit.Device.TXTData()) || !bytes.Equal(info.Key, key) || info.Version != 2 || info.Model != homekit.Device.Model() {
		t.Fatalf("inconsistent info identity: %+v", info)
	}
	if !bytes.Contains(info.TXT, []byte("vv=2")) {
		t.Fatal("TXT lacks protocol version")
	}
}
