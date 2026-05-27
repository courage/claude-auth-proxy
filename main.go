// Command claude-auth-proxy is a small reverse proxy that lets Claude Code
// running inside coding microVMs authenticate to a Claude Pro/Max subscription
// without the long-lived setup-token ever living on the VMs.
//
// The proxy holds the real token, strips the dummy Authorization header each VM
// sends, injects the real token, and forwards the request to api.anthropic.com.
// It joins the tailnet directly via tsnet so access is gated (and per-VM
// revocable) by Tailscale ACLs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"tailscale.com/tsnet"
)

func main() {
	var (
		stateDir  = flag.String("state-dir", "", "directory for tsnet persistent state")
		hostname  = flag.String("hostname", "claude-auth-proxy", "tsnet node hostname on the tailnet")
		listen    = flag.String("listen", ":8080", "listen address used in tsnet mode (e.g. :8080)")
		localAddr = flag.String("local-addr", "", "if set, listen on this plain TCP address and skip tsnet entirely (e.g. 127.0.0.1:8080) — for local/CI testing")
		tokenFile = flag.String("token-file", os.Getenv("CLAUDE_AUTH_PROXY_TOKEN_FILE"), "path to a file containing the Claude subscription setup-token (env fallback: CLAUDE_AUTH_PROXY_TOKEN_FILE)")
		upstream  = flag.String("upstream", "https://api.anthropic.com", "fixed upstream every request is forwarded to")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)

	token, err := loadToken(*tokenFile)
	if err != nil {
		logger.Fatalf("loading token: %v", err)
	}

	upstreamURL, err := url.Parse(*upstream)
	if err != nil {
		logger.Fatalf("parsing --upstream %q: %v", *upstream, err)
	}
	if upstreamURL.Scheme == "" || upstreamURL.Host == "" {
		logger.Fatalf("--upstream %q must be an absolute URL (scheme + host)", *upstream)
	}

	if *localAddr != "" {
		// Local/CI mode: plain TCP socket, no tailnet, no caller identity.
		handler := NewProxyHandler(upstreamURL, token, nil, logger)
		ln, err := net.Listen("tcp", *localAddr)
		if err != nil {
			logger.Fatalf("listen on %s: %v", *localAddr, err)
		}
		logger.Printf("claude-auth-proxy listening on %s (local mode, no tsnet) -> %s", *localAddr, upstreamURL)
		logger.Fatal(http.Serve(ln, handler))
	}

	// Default: join the tailnet via tsnet.
	ts := &tsnet.Server{
		Hostname: *hostname,
		Dir:      *stateDir,
		AuthKey:  os.Getenv("TS_AUTHKEY"),
	}
	defer ts.Close()

	ln, err := ts.Listen("tcp", *listen)
	if err != nil {
		logger.Fatalf("tsnet listen on %s: %v", *listen, err)
	}
	defer ln.Close()

	whois := newWhoIs(ts)
	handler := NewProxyHandler(upstreamURL, token, whois, logger)

	logger.Printf("claude-auth-proxy serving on tailnet as %q (listen %s) -> %s", *hostname, *listen, upstreamURL)
	logger.Fatal(http.Serve(ln, handler))
}

// loadToken reads the setup-token from path once at startup and trims trailing
// whitespace/newlines. It fails fast with a clear error if the path is unset,
// missing, or empty.
func loadToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("--token-file is required (or set CLAUDE_AUTH_PROXY_TOKEN_FILE)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading token file %q: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("token file %q is empty", path)
	}
	return token, nil
}

// newWhoIs returns an IdentityFunc backed by the tsnet LocalClient's WhoIs, for
// logging the calling tailnet node. It never fails the request: on any error it
// returns an empty identity.
func newWhoIs(ts *tsnet.Server) IdentityFunc {
	return func(ctx context.Context, remoteAddr string) string {
		lc, err := ts.LocalClient()
		if err != nil {
			return ""
		}
		who, err := lc.WhoIs(ctx, remoteAddr)
		if err != nil || who == nil {
			return ""
		}
		var node, login string
		if who.Node != nil {
			node = who.Node.Name
		}
		if who.UserProfile != nil {
			login = who.UserProfile.LoginName
		}
		switch {
		case node != "" && login != "":
			return fmt.Sprintf("%s (%s)", strings.TrimSuffix(node, "."), login)
		case node != "":
			return strings.TrimSuffix(node, ".")
		default:
			return login
		}
	}
}
