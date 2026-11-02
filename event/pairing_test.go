package event

import (
	"net"
	"testing"
	"time"
)

func resetPairings() {
	pairingMu.Lock()
	pairingKeys = map[string]pendingPairing{}
	pairingMu.Unlock()
}

func TestTakePairingSameAddress(t *testing.T) {
	resetPairings()
	RegisterPairing(&net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 7000}, []byte("a"))
	secret, host, ok := takePairing("2001:db8::1", time.Now())
	if !ok || string(secret) != "a" || host != "2001:db8::1" {
		t.Fatalf("got %q %q %t", secret, host, ok)
	}
	if _, _, ok := takePairing("2001:db8::1", time.Now()); ok {
		t.Fatal("a pairing must be used once")
	}
}

// A Mac opened its event channel from another of its IPv6 addresses than
// its RTSP connection; the channel was refused and the Mac never got one.
func TestTakePairingOtherAddressUsesLatest(t *testing.T) {
	resetPairings()
	RegisterPairing(&net.TCPAddr{IP: net.ParseIP("2001:db8::a"), Port: 7000}, []byte("old"))
	time.Sleep(time.Millisecond)
	RegisterPairing(&net.TCPAddr{IP: net.ParseIP("2001:db8::b"), Port: 7000}, []byte("new"))
	secret, host, ok := takePairing("2001:db8::c", time.Now())
	if !ok || string(secret) != "new" || host != "2001:db8::b" {
		t.Fatalf("got %q %q %t", secret, host, ok)
	}
}

func TestTakePairingIgnoresOldPairing(t *testing.T) {
	resetPairings()
	RegisterPairing(&net.TCPAddr{IP: net.ParseIP("2001:db8::a"), Port: 7000}, []byte("old"))
	if _, _, ok := takePairing("2001:db8::c", time.Now().Add(2*pairingMatchWindow)); ok {
		t.Fatal("a stale pairing must not be matched by another address")
	}
}
