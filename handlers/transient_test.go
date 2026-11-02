package handlers

import (
	"bytes"
	"io"
	"net"
	"testing"

	"github.com/brutella/hc/hap/pair"
	"github.com/brutella/hc/util"
	"goplay2/homekit"
	"goplay2/rtsp"
)

func TestTransientPairSetup(t *testing.T) {
	server, err := homekit.NewServer("02:00:00:00:00:02", "Test", t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	homekit.Server = server
	left, right := net.Pipe()
	defer right.Close()
	connection := &rtsp.Conn{}
	connection.SetNetConn(newSecureConn(left, server.Context))
	defer connection.Close()
	handler := &Rstp{transient: make(map[string]*pair.SetupServerSession)}

	m1 := util.NewTLV8Container()
	m1.SetByte(pair.TagPairingMethod, 0)
	m1.SetByte(pair.TagSequence, 1)
	m1.SetByte(tagFlags, pairFlagTransient)
	resp, err := handler.OnPairSetup(connection, &rtsp.Request{Body: m1.BytesBuffer().Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := util.NewTLV8ContainerFromReader(bytes.NewReader(resp.Body))
	if err != nil || m2.GetByte(pair.TagSequence) != 2 {
		t.Fatalf("bad M2: %v", err)
	}

	client := pair.NewSetupClientSession("Pair-Setup", transientPin)
	if err := client.GenerateKeys(m2.GetBytes(pair.TagSalt), m2.GetBytes(pair.TagPublicKey)); err != nil {
		t.Fatal(err)
	}
	m3 := util.NewTLV8Container()
	m3.SetByte(pair.TagSequence, 3)
	m3.SetBytes(pair.TagPublicKey, client.PublicKey)
	m3.SetBytes(pair.TagProof, client.Proof)
	resp, err = handler.OnPairSetup(connection, &rtsp.Request{Body: m3.BytesBuffer().Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	m4, err := util.NewTLV8ContainerFromReader(bytes.NewReader(resp.Body))
	if err != nil || m4.GetByte(pair.TagSequence) != 4 || m4.GetByte(pair.TagErrCode) != 0 {
		t.Fatalf("bad M4: %v", err)
	}
	if !client.IsServerProofValid(m4.GetBytes(pair.TagProof)) {
		t.Fatal("server proof rejected")
	}

	// Both sides must derive matching control channel keys from the SRP key.
	serverSide, err := newControlSession(client.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	clientSide := &controlSession{encryptKey: serverSide.decryptKey, decryptKey: serverSide.encryptKey}
	message := bytes.Repeat([]byte("SET_PARAMETER "), 200)
	encrypted, err := clientSide.Encrypt(bytes.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	for {
		chunk, err := serverSide.Decrypt(encrypted)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := io.Copy(&plain, chunk)
		if n == 0 {
			break
		}
	}
	if !bytes.Equal(plain.Bytes(), message) {
		t.Fatal("control channel round trip failed")
	}
}

func TestTransientPairSetupRejectsWrongPin(t *testing.T) {
	server, err := homekit.NewServer("02:00:00:00:00:03", "Test", t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	homekit.Server = server
	left, right := net.Pipe()
	defer right.Close()
	connection := &rtsp.Conn{}
	connection.SetNetConn(newSecureConn(left, server.Context))
	defer connection.Close()
	handler := &Rstp{transient: make(map[string]*pair.SetupServerSession)}
	m1 := util.NewTLV8Container()
	m1.SetByte(pair.TagSequence, 1)
	m1.SetByte(tagFlags, pairFlagTransient)
	resp, _ := handler.OnPairSetup(connection, &rtsp.Request{Body: m1.BytesBuffer().Bytes()})
	m2, _ := util.NewTLV8ContainerFromReader(bytes.NewReader(resp.Body))
	client := pair.NewSetupClientSession("Pair-Setup", "0000")
	if err := client.GenerateKeys(m2.GetBytes(pair.TagSalt), m2.GetBytes(pair.TagPublicKey)); err != nil {
		t.Fatal(err)
	}
	m3 := util.NewTLV8Container()
	m3.SetByte(pair.TagSequence, 3)
	m3.SetBytes(pair.TagPublicKey, client.PublicKey)
	m3.SetBytes(pair.TagProof, client.Proof)
	resp, _ = handler.OnPairSetup(connection, &rtsp.Request{Body: m3.BytesBuffer().Bytes()})
	m4, _ := util.NewTLV8ContainerFromReader(bytes.NewReader(resp.Body))
	if m4.GetByte(pair.TagErrCode) != errAuthentication {
		t.Fatal("wrong PIN was accepted")
	}
}
