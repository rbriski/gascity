package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// Remote-client transport budgets. A remote city is reached over a WAN, so the
// REST ceiling is generous (a federated read of a Dolt-backed rig store can take
// many seconds) while the dial and TLS handshakes are bounded tightly to fail
// fast on an unreachable host. The stream client has NO overall timeout — a
// long-lived SSE stream must not be capped — and is instead bounded by the
// per-frame idle watchdog in waitForEvent.
const (
	remoteRESTTimeout           = 120 * time.Second
	remoteDialTimeout           = 15 * time.Second
	remoteTLSHandshakeTimeout   = 15 * time.Second
	remoteResponseHeaderTimeout = 30 * time.Second
	remoteStreamIdleTimeout     = 45 * time.Second
)

// TokenSource yields a fresh transport bearer for each request and each SSE
// (re)connect. It is invoked live — never captured once — so a per-attempt 401
// re-mint takes effect. A nil TokenSource means no Authorization header is
// attached (a city that authenticates on X-GC-Request alone, or one fronted by
// a grant rather than a bearer).
type TokenSource func() (string, error)

// RemoteOptions configures the transport of a remote-city client.
type RemoteOptions struct {
	// Token, when non-nil, supplies the Authorization: Bearer <token> credential
	// (consumed by an edge/proxy; the controller ignores Authorization).
	Token TokenSource
	// CAFile is a PEM bundle used to verify the server certificate. Empty uses
	// the system roots.
	CAFile string
	// TLSServerName overrides the SNI / certificate name (for a host reached by
	// IP or through a fronting name).
	TLSServerName string
	// InsecureSkipVerify disables TLS verification (development only).
	InsecureSkipVerify bool
	// RESTTimeout overrides the overall REST timeout; 0 uses remoteRESTTimeout.
	// It is never applied to the SSE stream client.
	RESTTimeout time.Duration
}

// NewRemoteCityScopedClient builds a client that operates a REMOTE city at
// baseURL over the control plane. Unlike the local NewCityScopedClient, a
// malformed baseURL (or bad CA file) is a hard error at construction — a remote
// client is never a fallback-eligible stub. The returned client is marked
// isRemote so every error it produces is non-fallbackable (gate G1).
func NewRemoteCityScopedClient(baseURL, cityName string, opts RemoteOptions) (*Client, error) {
	rest, stream, err := newRemoteHTTPClients(opts)
	if err != nil {
		return nil, err
	}
	c := &Client{
		baseURL:      baseURL,
		cityName:     cityName,
		isRemote:     true,
		streamClient: stream,
		tokenSource:  opts.Token,
	}
	cw, err := genclient.NewClientWithResponses(
		baseURL,
		genclient.WithHTTPClient(rest),
		genclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-GC-Request", "true")
			return nil
		}),
		genclient.WithRequestEditorFn(remoteAuthEditor(c)),
	)
	if err != nil {
		return nil, fmt.Errorf("building remote client for %q: %w", baseURL, err)
	}
	c.cw = cw
	return c, nil
}

// remoteAuthEditor returns a genclient request editor that attaches a fresh
// bearer (from the client's token source) to every REST request. It closes over
// the client so the token is fetched live, not captured at construction.
func remoteAuthEditor(c *Client) genclient.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		tok, err := c.bearerToken()
		if err != nil {
			return err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return nil
	}
}

// newRemoteHTTPClients builds the two client shapes from a single TLS/redirect
// policy: a REST client (bounded overall timeout + tight dial/TLS budgets) and a
// stream client (no overall timeout; idle-bounded by the caller). Both refuse
// credential-leaking redirects.
func newRemoteHTTPClients(opts RemoteOptions) (rest, stream *http.Client, err error) {
	tlsCfg, err := remoteTLSConfig(opts)
	if err != nil {
		return nil, nil, err
	}
	newTransport := func() *http.Transport {
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   remoteDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSClientConfig:       tlsCfg,
			TLSHandshakeTimeout:   remoteTLSHandshakeTimeout,
			ResponseHeaderTimeout: remoteResponseHeaderTimeout,
			ForceAttemptHTTP2:     true,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	restTimeout := opts.RESTTimeout
	if restTimeout <= 0 {
		restTimeout = remoteRESTTimeout
	}
	rest = &http.Client{
		Timeout:       restTimeout,
		Transport:     newTransport(),
		CheckRedirect: remoteCheckRedirect,
	}
	stream = &http.Client{
		Timeout:       0, // never cap a long-lived SSE stream; see remoteStreamIdleTimeout
		Transport:     newTransport(),
		CheckRedirect: remoteCheckRedirect,
	}
	return rest, stream, nil
}

// remoteTLSConfig builds the client TLS config from the options: a custom CA
// bundle, an SNI/name override, and (dev-only) verification skip. MinVersion is
// pinned to TLS 1.2.
func remoteTLSConfig(opts RemoteOptions) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TLSServerName != "" {
		cfg.ServerName = opts.TLSServerName
	}
	if opts.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading ca_file %q: %w", opts.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %q: no valid PEM certificates found", opts.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// remoteCheckRedirect is the redirect policy for every remote request. It
// refuses a cross-host redirect and an https->http downgrade outright (either
// could exfiltrate a bearer/grant or drop it onto plaintext), and — defense in
// depth — strips the Authorization and every X-GC-* header from any redirect it
// does allow (a same-host, same-or-upgraded-scheme hop).
func remoteCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	orig := via[0].URL
	if !strings.EqualFold(req.URL.Host, orig.Host) {
		return fmt.Errorf("refusing cross-host redirect from %s to %s (credentials are per-host)", orig.Host, req.URL.Host)
	}
	if orig.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing https->%s downgrade redirect to %s", req.URL.Scheme, req.URL.Host)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}
	stripSensitiveHeaders(req.Header)
	return nil
}

// stripSensitiveHeaders removes the Authorization header and every X-GC-*
// control/grant header from h. Header map keys are already canonicalized by
// net/http (X-GC-Request -> X-Gc-Request), so a case-insensitive x-gc- prefix
// test catches them all.
func stripSensitiveHeaders(h http.Header) {
	h.Del("Authorization")
	for key := range h {
		if strings.HasPrefix(strings.ToLower(key), "x-gc-") {
			h.Del(key)
		}
	}
}
