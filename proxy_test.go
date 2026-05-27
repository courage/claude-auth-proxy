package main

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes the proxy's
// request logging performs from its handler goroutine while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForLog waits for the proxy's per-request log line, which is written after
// ServeHTTP returns and therefore races the client. Polls briefly, then fails.
func waitForLog(t *testing.T, b *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := b.String(); s != "" {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected a log line for the request")
	return ""
}

const (
	dummyToken = "sk-ant-oat01-DUMMY-from-vm"
	realToken  = "sk-ant-oat01-REAL-secret-token-value"
)

// captured records what the fake upstream received.
type captured struct {
	method     string
	requestURI string
	header     http.Header
	body       string
}

// newFakeUpstream returns an httptest server that records the request it
// received into *got and responds with respStatus/respBody.
func newFakeUpstream(t *testing.T, got *captured, respStatus int, respBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.method = r.Method
		got.requestURI = r.URL.RequestURI()
		got.header = r.Header.Clone()
		got.body = string(body)
		w.WriteHeader(respStatus)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newProxyServer wires NewProxyHandler in front of upstreamURL, logging to
// logBuf, and returns an httptest server fronting it.
func newProxyServer(t *testing.T, upstreamURL string, logBuf *syncBuffer) *httptest.Server {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	logger := log.New(logBuf, "", 0)
	proxy := httptest.NewServer(NewProxyHandler(u, realToken, nil, logger))
	t.Cleanup(proxy.Close)
	return proxy
}

func TestProxyRewritesAuthAndForwardsEverythingElse(t *testing.T) {
	var got captured
	upstream := newFakeUpstream(t, &got, http.StatusOK, `{"ok":true}`)

	var logBuf syncBuffer
	proxy := newProxyServer(t, upstream.URL, &logBuf)

	reqBody := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages?beta=true", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	// Headers a real claude 2.1.x request carries.
	req.Header.Set("Authorization", "Bearer "+dummyToken)
	req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-app", "cli")
	req.Header.Set("user-agent", "claude-cli/2.1.0 (external, sdk-cli)")
	req.Header.Set("x-stainless-arch", "arm64")
	req.Header.Set("x-stainless-os", "Linux")
	req.Header.Set("x-claude-code-session-id", "abc-123")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// Authorization rewritten to the real token.
	if gotAuth := got.header.Get("Authorization"); gotAuth != "Bearer "+realToken {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+realToken)
	}
	if strings.Contains(got.header.Get("Authorization"), dummyToken) {
		t.Error("dummy token leaked through to upstream")
	}

	// Path + query preserved verbatim.
	if got.requestURI != "/v1/messages?beta=true" {
		t.Errorf("requestURI = %q, want %q", got.requestURI, "/v1/messages?beta=true")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}

	// Body unchanged.
	if got.body != reqBody {
		t.Errorf("body = %q, want %q", got.body, reqBody)
	}

	// Every other header forwarded unchanged.
	for _, h := range []struct{ k, v string }{
		{"Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14"},
		{"Anthropic-Version", "2023-06-01"},
		{"X-App", "cli"},
		{"User-Agent", "claude-cli/2.1.0 (external, sdk-cli)"},
		{"X-Stainless-Arch", "arm64"},
		{"X-Stainless-Os", "Linux"},
		{"X-Claude-Code-Session-Id", "abc-123"},
	} {
		if v := got.header.Get(h.k); v != h.v {
			t.Errorf("header %s = %q, want %q", h.k, v, h.v)
		}
	}
}

func TestProxyAlwaysHitsFixedUpstream(t *testing.T) {
	// Not an open relay: even with a bogus Host header, the request must reach
	// our configured upstream (this fake), never the requested host.
	var got captured
	upstream := newFakeUpstream(t, &got, http.StatusOK, "ok")

	var logBuf syncBuffer
	proxy := newProxyServer(t, upstream.URL, &logBuf)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	req.Host = "evil.example.com"
	req.Header.Set("Authorization", "Bearer "+dummyToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got.requestURI != "/v1/models" {
		t.Fatalf("request never reached the fixed upstream; requestURI=%q", got.requestURI)
	}
	// And the upstream saw the upstream's own host, not evil.example.com.
	if h := got.header.Get("Host"); strings.Contains(h, "evil.example.com") {
		t.Errorf("forwarded Host leaked caller value: %q", h)
	}
}

func TestProxyPropagatesNon2xx(t *testing.T) {
	var got captured
	upstream := newFakeUpstream(t, &got, http.StatusTooManyRequests, `{"error":"slow down"}`)

	var logBuf syncBuffer
	proxy := newProxyServer(t, upstream.URL, &logBuf)

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+dummyToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	if string(body) != `{"error":"slow down"}` {
		t.Errorf("body = %q, want upstream error body", string(body))
	}
}

func TestProxyStreamsSSEIncrementally(t *testing.T) {
	// The upstream sends the first SSE chunk, flushes, then blocks until the
	// client has actually received it. If the proxy buffered the response, the
	// client read would block and this test would time out.
	released := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		<-released // only proceed once the client confirms it got chunk 1
		io.WriteString(w, "data: second\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	var logBuf syncBuffer
	proxy := newProxyServer(t, upstream.URL, &logBuf)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+dummyToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)

	type readResult struct {
		line string
		err  error
	}
	first := make(chan readResult, 1)
	go func() {
		line, err := reader.ReadString('\n')
		first <- readResult{line, err}
	}()

	select {
	case r := <-first:
		if r.err != nil {
			t.Fatalf("reading first chunk: %v", r.err)
		}
		if !strings.Contains(r.line, "first") {
			t.Fatalf("first chunk = %q, want it to contain \"first\"", r.line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive first SSE chunk before upstream sent the second — response was buffered, not streamed")
	}

	// Let the upstream send the rest and confirm we get it.
	close(released)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading rest: %v", err)
	}
	if !strings.Contains(string(rest), "second") {
		t.Errorf("did not receive second chunk; got %q", string(rest))
	}
}

func TestProxyNeverLogsSecrets(t *testing.T) {
	var got captured
	upstream := newFakeUpstream(t, &got, http.StatusOK, "ok")

	var logBuf syncBuffer
	proxy := newProxyServer(t, upstream.URL, &logBuf)

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages?beta=true", strings.NewReader("secret-body-contents"))
	req.Header.Set("Authorization", "Bearer "+dummyToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	logs := waitForLog(t, &logBuf)
	for _, secret := range []string{realToken, dummyToken, "Bearer", "secret-body-contents"} {
		if strings.Contains(logs, secret) {
			t.Errorf("log output leaked %q:\n%s", secret, logs)
		}
	}
	// Sanity: the log line should still carry the useful, non-secret fields.
	if !strings.Contains(logs, "/v1/messages?beta=true") || !strings.Contains(logs, "status=200") {
		t.Errorf("log line missing expected fields:\n%s", logs)
	}
}

func TestLoadToken(t *testing.T) {
	t.Run("trims whitespace", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(f, []byte("  the-token\n\n"), 0600); err != nil {
			t.Fatal(err)
		}
		tok, err := loadToken(f)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tok != "the-token" {
			t.Errorf("token = %q, want %q", tok, "the-token")
		}
	})

	t.Run("empty path", func(t *testing.T) {
		if _, err := loadToken(""); err == nil {
			t.Fatal("expected error for empty path")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := loadToken(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "empty")
		if err := os.WriteFile(f, []byte("   \n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadToken(f); err == nil {
			t.Fatal("expected error for whitespace-only file")
		}
	})
}
