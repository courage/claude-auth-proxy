# Testing `claude-auth-proxy`

A runnable test plan. Sections 1–2 need no tailnet and run in CI. Sections 3–4
are manual and need real credentials. Section 5 is negative/hardening.

> **Go toolchain note.** Use the flake dev shell (`nix develop`, or `direnv allow`)
> for a pinned Go. The build is pure-Go, so set `CGO_ENABLED=0` when invoking Go
> directly (the dev shell ships no C compiler, and the Nix package sets this for
> you). All `go` commands below assume `CGO_ENABLED=0` is exported.

## 1. Build / static checks

```sh
# Nix build (computes/pins vendorHash, produces ./result/bin/claude-auth-proxy)
nix build

# Go build, vet, format, in the dev shell
export CGO_ENABLED=0
go build ./...
go vet ./...
gofmt -l .          # must print nothing
go test ./...       # unit tests, see §2
```

Expected: `nix build` succeeds; `go build`/`go vet` are silent; `gofmt -l`
prints nothing; `go test ./...` reports `ok`.

## 2. Unit tests (no tailnet)

`proxy_test.go` exercises the proxy handler against an `httptest` fake upstream
that records exactly what it received. Run:

```sh
CGO_ENABLED=0 go test -v -run TestProxy ./...
CGO_ENABLED=0 go test -v -run TestLoadToken ./...
```

Coverage maps directly to the requirements:

| Test | Asserts |
| ---- | ------- |
| `TestProxyRewritesAuthAndForwardsEverythingElse` | Incoming `Authorization: Bearer <dummy>` is replaced by the real token; `anthropic-beta`, `anthropic-version`, `x-app`, `user-agent`, `x-stainless-*`, `x-claude-code-session-id` arrive **unchanged**; method, path + query (`/v1/messages?beta=true`), and request body are preserved verbatim. |
| `TestProxyAlwaysHitsFixedUpstream` | A request with a bogus `Host` still reaches **only** the configured upstream (not an open relay), and the caller's Host is not forwarded as the upstream Host. |
| `TestProxyPropagatesNon2xx` | An upstream `429` (status + body) is propagated to the client unchanged. |
| `TestProxyStreamsSSEIncrementally` | The first SSE chunk reaches the client **before** the upstream sends the second (the upstream blocks until the client confirms receipt). If the proxy buffered the response, the test times out — proving `FlushInterval = -1` streaming works through the response wrapper. |
| `TestProxyNeverLogsSecrets` | The log output contains neither the real token, the dummy token, the literal `Bearer`, nor the request body — but still carries the useful fields (`/v1/messages?beta=true`, `status=200`). |
| `TestLoadToken` | Whitespace is trimmed; empty `--token-file` path, missing file, and whitespace-only file each fail fast with an error. |

### Quick smoke test of the built binary (optional, no tailnet)

```sh
printf 'sk-ant-oat01-REALTOKEN\n' > /tmp/token.txt

# Trivial fake upstream that echoes the request it received:
# (any tool works; here we just confirm the rewrite with a one-shot listener)
./result/bin/claude-auth-proxy \
  --local-addr 127.0.0.1:9900 \
  --upstream http://127.0.0.1:9901 \
  --token-file /tmp/token.txt &

curl -s -X POST \
  -H 'Authorization: Bearer sk-ant-oat01-DUMMY' \
  -H 'anthropic-version: 2023-06-01' -H 'x-app: cli' \
  --data '{"hi":1}' \
  'http://127.0.0.1:9900/v1/messages?beta=true'
```

The upstream should observe `Authorization: Bearer sk-ant-oat01-REALTOKEN`,
path `/v1/messages?beta=true`, the unchanged body, and `Host` set to the
upstream. The proxy's log line shows `method=POST path=/v1/messages?beta=true`
with **no** token.

## 3. Integration vs real Anthropic (manual — needs a real setup-token)

Run in `--local-addr` mode pointing at a real token, then drive Claude Code with
a dummy token:

```sh
printf '%s\n' "$REAL_SETUP_TOKEN" > /tmp/real-token.txt
./result/bin/claude-auth-proxy --local-addr 127.0.0.1:8080 --token-file /tmp/real-token.txt &

ANTHROPIC_BASE_URL=http://127.0.0.1:8080 \
CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-DUMMY \
claude -p "reply with exactly the single word: pong"
```

Expected: Claude replies `pong`. The proxy log shows the request method/path and
a `200` upstream status, and contains **no** token. (This exact flow is known to
work — the setup-token is not device/IP-bound and bills the subscription.)

Clean up `/tmp/real-token.txt` afterward.

## 4. tsnet / tailnet (manual — needs a real tagged TS_AUTHKEY)

```sh
export TS_AUTHKEY=tskey-auth-...      # tagged key for tag:claude-proxy
./result/bin/claude-auth-proxy \
  --token-file /tmp/real-token.txt \
  --state-dir /tmp/cap-state \
  --listen :8080
```

Checks:

1. The node appears in the tailnet admin console under `tag:claude-proxy` with
   hostname `claude-auth-proxy`.
2. From a **tagged** client allowed by ACL:
   ```sh
   ANTHROPIC_BASE_URL=http://<proxy-tailnet-ip-or-name>:8080 \
   CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-DUMMY \
   claude -p "reply with exactly the single word: pong"
   ```
   Expect `pong`; the proxy log shows the caller's tailnet identity via `WhoIs`.
3. From an **untagged**/disallowed node, the connection is denied by the
   Tailscale ACL (it never reaches the proxy handler).

## 5. Negative / hardening

- **Open-relay check:** send a request with an arbitrary `Host` header (e.g.
  `Host: evil.example.com`) and a path; confirm it is still forwarded only to
  `api.anthropic.com` (covered automatically by `TestProxyAlwaysHitsFixedUpstream`,
  and observable in §3 by watching where traffic actually lands).
- **No `Authorization` leakage in logs:** grep the proxy's stderr for the token,
  `Bearer`, or `TS_AUTHKEY`; expect no matches (covered by
  `TestProxyNeverLogsSecrets`). Example:
  ```sh
  ./result/bin/claude-auth-proxy ... 2> proxy.log
  grep -E 'Bearer|sk-ant-oat|tskey-auth' proxy.log   # expect no matches
  ```
- **Missing/empty token:** start with a missing or empty `--token-file`; expect
  an immediate, clear fatal error and a non-zero exit (covered by `TestLoadToken`):
  ```sh
  ./result/bin/claude-auth-proxy --local-addr 127.0.0.1:8080 --token-file /nonexistent
  # -> "loading token: reading token file ...: ... no such file or directory", exit 1
  ./result/bin/claude-auth-proxy --local-addr 127.0.0.1:8080
  # -> "loading token: --token-file is required (or set CLAUDE_AUTH_PROXY_TOKEN_FILE)", exit 1
  ```
- **Unreachable / slow upstream:** point `--upstream` at a dead address; a client
  request returns `502 Bad Gateway` and the proxy logs a `proxy error: ... err=...`
  line plus a `status=502` line. Because the transport bounds dial/TLS/response-
  header waits but imposes **no** overall response deadline, a long but live
  streaming completion is never cut off.
