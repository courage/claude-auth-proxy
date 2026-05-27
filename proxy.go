package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// IdentityFunc resolves a connection's remote address ("ip:port") to a
// human-readable tailnet identity. It returns "" when the identity can't be
// determined (or when not running on a tailnet). In tsnet mode this is backed
// by the LocalClient's WhoIs; in --local-addr mode it is nil.
type IdentityFunc func(ctx context.Context, remoteAddr string) string

// newProxyTransport returns a transport with connection-establishment
// timeouts but deliberately no overall response deadline, so long streaming
// completions are never cut off mid-stream.
//
// DisableCompression keeps the proxy from injecting its own Accept-Encoding or
// transparently decompressing/recompressing bodies: whatever the client sent
// is forwarded verbatim and whatever the upstream returns is streamed back as
// received.
func newProxyTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Bounds only the wait for the response *headers*, not the body, so a
		// slow-to-start completion is allowed but a dead upstream still fails.
		ResponseHeaderTimeout: 60 * time.Second,
		DisableCompression:    true,
	}
}

// NewProxyHandler builds the reverse-proxy handler. Every request is forwarded
// to the fixed upstream regardless of its inbound Host (this is not an open
// relay); the inbound Authorization header is dropped and replaced with the
// real subscription token; the path, query, body, and all other headers are
// forwarded unchanged. Responses (including SSE) stream through immediately.
//
// whois may be nil. logger must be non-nil.
func NewProxyHandler(upstream *url.URL, token string, whois IdentityFunc, logger *log.Logger) http.Handler {
	rp := &httputil.ReverseProxy{
		// -1 disables output buffering so each upstream chunk (e.g. an SSE
		// completion delta) is flushed to the client as it arrives.
		FlushInterval: -1,
		Transport:     newProxyTransport(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Pin the destination to the configured upstream no matter what
			// Host/URL the caller sent. pr.Out.URL.Path/RawQuery are inherited
			// from the inbound request, so they're forwarded verbatim.
			pr.Out.URL.Scheme = upstream.Scheme
			pr.Out.URL.Host = upstream.Host
			pr.Out.Host = upstream.Host

			// The one and only header we rewrite.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set("Authorization", "Bearer "+token)
		},
		ErrorLog: logger,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Printf("proxy error: method=%s path=%s err=%v", r.Method, r.URL.RequestURI(), err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &responseRecorder{ResponseWriter: w}
		rp.ServeHTTP(rec, r)

		identity := ""
		if whois != nil {
			identity = whois(r.Context(), r.RemoteAddr)
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		// Structured, single line per request. Never includes the token, the
		// Authorization header, or any body.
		logger.Printf("method=%s path=%s status=%d bytes=%d remote=%s identity=%q",
			r.Method, r.URL.RequestURI(), status, rec.bytes, r.RemoteAddr, identity)
	})
}

// responseRecorder wraps an http.ResponseWriter to capture the status code and
// number of body bytes written, while preserving the Flusher behaviour that
// ReverseProxy relies on for streaming.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (rr *responseRecorder) WriteHeader(code int) {
	if rr.status == 0 {
		rr.status = code
	}
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if rr.status == 0 {
		rr.status = http.StatusOK
	}
	n, err := rr.ResponseWriter.Write(b)
	rr.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer so FlushInterval = -1 streaming works
// through the wrapper.
func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
