package dacp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestCommandSendsActiveRemote(t *testing.T) {
	var path, remote string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, remote = r.URL.Path, r.Header.Get("Active-Remote")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	r := New("127.0.0.1", "ABCDEF0123456789", "1234567890")
	lookups := 0
	r.lookup = func(ctx context.Context, id string) (int, error) {
		lookups++
		if id != "ABCDEF0123456789" {
			t.Fatalf("looked up %q", id)
		}
		return port, nil
	}
	if err := r.Command(4); err != nil { // next
		t.Fatal(err)
	}
	if path != "/ctrl-int/1/nextitem" || remote != "1234567890" {
		t.Fatalf("request %s Active-Remote=%s", path, remote)
	}
	if err := r.Command(1); err != nil || path != "/ctrl-int/1/pause" {
		t.Fatalf("pause: %v %s", err, path)
	}
	if lookups != 1 {
		t.Fatalf("control port looked up %d times", lookups)
	}
	if err := r.Command(99); err == nil {
		t.Fatal("unknown command accepted")
	}
}
