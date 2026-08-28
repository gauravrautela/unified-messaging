package safehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientRefusesLoopbackUnlessAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	c := Client(2 * time.Second)
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("loopback dial must be refused by default")
	}
	AllowLoopbackForTests(t)
	resp, err := Client(2 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	AllowLoopbackForTests(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	resp, err := Client(2 * time.Second).Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 307 || hits != 1 {
		t.Fatalf("status %d hits %d: redirect was followed", resp.StatusCode, hits)
	}
}

func TestPublicOnlyControlRejectsPrivateRanges(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:80", "10.1.2.3:443", "172.16.0.1:80", "192.168.1.1:80",
		"169.254.169.254:80", "100.64.0.1:80", "[::1]:80", "[fe80::1]:80", "[::ffff:10.0.0.1]:80", "0.0.0.0:80"} {
		if err := PublicOnlyControl("tcp", addr, nil); err == nil {
			t.Errorf("%s: expected refusal", addr)
		}
	}
	for _, addr := range []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443"} {
		if err := PublicOnlyControl("tcp", addr, nil); err != nil {
			t.Errorf("%s: unexpected refusal: %v", addr, err)
		}
	}
}
