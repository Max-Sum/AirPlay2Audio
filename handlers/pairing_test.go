package handlers

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/homekit"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
	"net"
	"strings"
	"testing"

	"github.com/brutella/hc/hap"
	"github.com/brutella/hc/hap/pair"
	"github.com/brutella/hc/util"
)

func TestPairVerifyCanRestartOnSameConnection(t *testing.T) {
	server, err := homekit.NewServer("02:00:00:00:00:01", "Test", t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	homekit.Server = server
	left, right := net.Pipe()
	defer right.Close()
	connection := &rtsp.Conn{}
	connection.SetNetConn(newSecureConn(left, server.Context))
	defer connection.Close()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	container := util.NewTLV8Container()
	container.SetByte(pair.TagSequence, byte(pair.VerifyStepStartRequest))
	container.SetBytes(pair.TagPublicKey, key.PublicKey().Bytes())
	req := &rtsp.Request{Body: container.BytesBuffer().Bytes()}
	handler := &Rstp{}
	for attempt := 0; attempt < 2; attempt++ {
		response, err := handler.OnPairVerify(connection, req)
		if err != nil || response.StatusCode != rtsp.StatusOK {
			t.Fatalf("attempt %d: status=%v err=%v", attempt, response, err)
		}
		payload, err := util.NewTLV8ContainerFromReader(bytes.NewReader(response.Body))
		if err != nil || payload.GetByte(pair.TagSequence) != byte(pair.VerifyStepStartResponse) {
			t.Fatalf("invalid verify response: %v", err)
		}
	}
}

type rejectedVerify struct{}

func (rejectedVerify) SharedKey() [32]byte { return [32]byte{1} }
func (rejectedVerify) Handle(util.Container) (util.Container, error) {
	out := util.NewTLV8Container()
	out.SetByte(pair.TagSequence, byte(pair.VerifyStepFinishResponse))
	out.SetByte(pair.TagErrCode, 4)
	return out, nil
}

func TestRejectedPairVerifyDoesNotAuthenticate(t *testing.T) {
	server, err := homekit.NewServer("02:00:00:00:00:01", "Test", t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	homekit.Server = server
	left, right := net.Pipe()
	defer right.Close()
	connection := &rtsp.Conn{}
	connection.SetNetConn(newSecureConn(left, server.Context))
	defer connection.Close()
	session := server.Context.Get(connection.NetConn().RemoteAddr().String()).(hap.Session)
	session.SetPairVerifyHandler(rejectedVerify{})
	container := util.NewTLV8Container()
	container.SetByte(pair.TagSequence, byte(pair.VerifyStepFinishRequest))
	container.SetBytes(pair.TagEncryptedData, make([]byte, 16))
	response, err := (&Rstp{}).OnPairVerify(connection, &rtsp.Request{Body: container.BytesBuffer().Bytes()})
	if err != nil || response.StatusCode != rtsp.StatusOK {
		t.Fatalf("verify: %v %+v", err, response)
	}
	if session.Decrypter() != nil {
		t.Fatal("failed verification enabled encryption")
	}
}

func TestPairingManagementNeedsPairVerify(t *testing.T) {
	handler := &Rstp{}
	connection := &rtsp.Conn{}
	for _, path := range []string{"pair-add", "pair-remove", "pair-list"} {
		response, err := handler.OnPostWeb(connection, &rtsp.Request{Method: "POST", Path: path})
		if err != nil || response.StatusCode != rtsp.StatusConnectionAuthorizationRequired {
			t.Fatalf("%s without pair-verify: %v %+v", path, err, response)
		}
	}
}

func TestAdvertisementUsesAccessoryPublicKey(t *testing.T) {
	server, err := homekit.NewServer("02:00:00:00:00:02", "Test", t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	device := homekit.NewAccessory("02:00:00:00:00:02", "00000000-0000-0000-0000-000000000001", globals.NewFeatures())
	device.SetPublicKey(server.Device.PublicKey())
	want := "pk=" + hex.EncodeToString(server.Device.PublicKey())
	if !strings.Contains(strings.Join(device.ToRecords(), "\n"), want) {
		t.Fatalf("advertised key does not match accessory")
	}
}
