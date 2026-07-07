package api

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeServerCA writes an httptest TLS server's certificate to a PEM file and
// returns its path, so a client can verify the self-signed server.
func writeServerCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test server has no certificate")
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRemoteCheckRedirect(t *testing.T) {
	mkReq := func(rawurl string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, rawurl, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	orig := mkReq("https://box.internal:9443/v0/city/mc/status")

	t.Run("no via is allowed", func(t *testing.T) {
		if err := remoteCheckRedirect(mkReq("https://box.internal:9443/x"), nil); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("cross-host refused", func(t *testing.T) {
		err := remoteCheckRedirect(mkReq("https://evil.example.com/x"), []*http.Request{orig})
		if err == nil {
			t.Fatal("cross-host redirect must be refused")
		}
	})
	t.Run("https->http downgrade refused", func(t *testing.T) {
		err := remoteCheckRedirect(mkReq("http://box.internal:9443/x"), []*http.Request{orig})
		if err == nil {
			t.Fatal("downgrade redirect must be refused")
		}
	})
	t.Run("same-host allowed and strips creds", func(t *testing.T) {
		next := mkReq("https://box.internal:9443/other")
		next.Header.Set("Authorization", "Bearer secret")
		next.Header.Set("X-GC-Request", "true")
		next.Header.Set("X-GC-City-Write", "grant")
		next.Header.Set("Accept", "text/event-stream")
		if err := remoteCheckRedirect(next, []*http.Request{orig}); err != nil {
			t.Fatalf("same-host redirect should be allowed: %v", err)
		}
		if next.Header.Get("Authorization") != "" {
			t.Error("Authorization must be stripped on a followed redirect")
		}
		if next.Header.Get("X-GC-Request") != "" || next.Header.Get("X-GC-City-Write") != "" {
			t.Error("X-GC-* headers must be stripped on a followed redirect")
		}
		if next.Header.Get("Accept") != "text/event-stream" {
			t.Error("non-sensitive headers must be preserved")
		}
	})
	t.Run("too many redirects refused", func(t *testing.T) {
		via := make([]*http.Request, 10)
		for i := range via {
			via[i] = orig
		}
		if err := remoteCheckRedirect(mkReq("https://box.internal:9443/x"), via); err == nil {
			t.Fatal("must stop after too many redirects")
		}
	})
}

func TestRemoteTLSConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := remoteTLSConfig(RemoteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinVersion != tls.VersionTLS12 || cfg.RootCAs != nil || cfg.InsecureSkipVerify {
			t.Errorf("unexpected default cfg: %+v", cfg)
		}
	})
	t.Run("server name + insecure propagate", func(t *testing.T) {
		cfg, err := remoteTLSConfig(RemoteOptions{TLSServerName: "box", InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ServerName != "box" || !cfg.InsecureSkipVerify {
			t.Errorf("cfg = %+v", cfg)
		}
	})
	t.Run("missing ca file errors", func(t *testing.T) {
		if _, err := remoteTLSConfig(RemoteOptions{CAFile: "/no/such/ca.pem"}); err == nil {
			t.Fatal("missing ca_file must error")
		}
	})
	t.Run("garbage ca file errors", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bad.pem")
		if err := os.WriteFile(p, []byte("not a pem"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := remoteTLSConfig(RemoteOptions{CAFile: p}); err == nil {
			t.Fatal("garbage ca_file must error")
		}
	})
}

func TestRemoteAuthEditor(t *testing.T) {
	t.Run("attaches bearer", func(t *testing.T) {
		c := &Client{tokenSource: func() (string, error) { return "tok123", nil }}
		req, _ := http.NewRequest(http.MethodGet, "https://h/x", nil)
		if err := remoteAuthEditor(c)(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("Authorization = %q", got)
		}
	})
	t.Run("nil source attaches nothing", func(t *testing.T) {
		c := &Client{}
		req, _ := http.NewRequest(http.MethodGet, "https://h/x", nil)
		if err := remoteAuthEditor(c)(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Authorization") != "" {
			t.Error("no bearer must be attached without a token source")
		}
	})
	t.Run("source error propagates", func(t *testing.T) {
		c := &Client{tokenSource: func() (string, error) { return "", errors.New("mint failed") }}
		req, _ := http.NewRequest(http.MethodGet, "https://h/x", nil)
		if err := remoteAuthEditor(c)(context.Background(), req); err == nil {
			t.Fatal("token source error must propagate")
		}
	})
}

func TestNewRemoteCityScopedClient_Basics(t *testing.T) {
	c, err := NewRemoteCityScopedClient("https://box:9443", "mc", RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsRemote() {
		t.Error("client must be marked remote")
	}
	if c.cityName != "mc" || c.streamClient == nil {
		t.Errorf("client not wired: cityName=%q streamClient=%v", c.cityName, c.streamClient)
	}
	// A bad CA file is a hard error at construction (never a fallback stub).
	if _, err := NewRemoteCityScopedClient("https://box:9443", "mc", RemoteOptions{CAFile: "/no/such"}); err == nil {
		t.Fatal("bad ca_file must fail construction")
	}
}

// End-to-end over TLS: the REST shape verifies the server against a supplied CA,
// fails without it, succeeds with InsecureSkipVerify, and delivers both the
// X-GC-Request and Authorization headers.
func TestNewRemoteCityScopedClient_TLSAndHeaders(t *testing.T) {
	var gotAuth, gotReq string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotReq = r.Header.Get("X-GC-Request")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	caPath := writeServerCA(t, srv)

	t.Run("verified with ca, headers delivered", func(t *testing.T) {
		gotAuth, gotReq = "", ""
		c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{
			CAFile: caPath,
			Token:  func() (string, error) { return "tok123", nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetStatus(); err != nil {
			// A decode/shape error is fine; a transport/TLS error is not.
			if IsConnError(err) {
				t.Fatalf("TLS/transport error with valid CA: %v", err)
			}
		}
		if gotReq != "true" {
			t.Errorf("X-GC-Request = %q, want true", gotReq)
		}
		if gotAuth != "Bearer tok123" {
			t.Errorf("Authorization = %q, want Bearer tok123", gotAuth)
		}
	})

	t.Run("fails without ca", func(t *testing.T) {
		c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetStatus(); err == nil || !IsConnError(err) {
			t.Fatalf("expected a TLS/transport error without CA, got %v", err)
		}
	})

	t.Run("insecure skip verify succeeds", func(t *testing.T) {
		gotReq = ""
		c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetStatus(); err != nil && IsConnError(err) {
			t.Fatalf("insecure_skip_verify should connect: %v", err)
		}
		if gotReq != "true" {
			t.Errorf("request did not reach server (X-GC-Request=%q)", gotReq)
		}
	})
}

// A cross-host redirect must be refused end-to-end (the second host never
// receives the request, so a bearer cannot leak to it).
func TestRemoteClient_RefusesCrossHostRedirect(t *testing.T) {
	var reachedSecond bool
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedSecond = true
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/v0/city/mc/status", http.StatusFound)
	}))
	defer first.Close()

	c, err := NewRemoteCityScopedClient(first.URL, "mc", RemoteOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetStatus()
	if err == nil || !IsConnError(err) {
		t.Fatalf("cross-host redirect must fail as a transport error, got %v", err)
	}
	if reachedSecond {
		t.Fatal("request must NOT reach the cross-host redirect target")
	}
}
