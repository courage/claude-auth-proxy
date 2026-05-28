# claude-auth-proxy

A small Go reverse proxy that lets **Claude Code running inside coding microVMs**
authenticate to a Claude Pro/Max **subscription** without the long-lived OAuth
setup-token ever living on the VMs.

The proxy holds the real token, strips the dummy `Authorization` header each VM
sends, injects the real token, and forwards the request to
`https://api.anthropic.com`. It joins the tailnet directly via
[`tsnet`](https://tailscale.com/kb/1244/tsnet), so access is gated by Tailscale
ACLs and is therefore **per-VM revocable** — revoke a VM by removing its tailnet
tag; no token rotation needed.

```
Claude Code in VM  --http (over tailnet/WireGuard)-->  claude-auth-proxy (tsnet node)
   sends DUMMY token                                      strips dummy Authorization,
                                                          injects REAL setup-token,
                                                          forwards over TLS to
                                                          https://api.anthropic.com
```

## Why

Subscription auth's only headless credential is a ~1-year `setup-token`
(`CLAUDE_CODE_OAUTH_TOKEN`). There is no short-lived or per-token-revocable
variant, and switching to API-key billing is not wanted. Shipping that token
into every agent VM would put a long-lived, account-wide credential in the
least-trusted box with no per-VM revocation. The broker/proxy pattern fixes
that: the token lives on **one** trusted node; VMs send a throwaway dummy token;
the proxy swaps in the real one.

## What it does to each request

- Always forwards to the fixed `--upstream` (default `https://api.anthropic.com`)
  **regardless of the request's Host header** — this is **not** an open relay.
- Sets the outbound `Host` to the upstream host.
- **Deletes any incoming `Authorization` header and sets
  `Authorization: Bearer <token>`.** This is the only header it rewrites.
- Forwards path + query verbatim (e.g. `/v1/messages?beta=true`).
- Forwards all other request headers and the body unchanged; does not
  decompress/recompress.
- Streams responses (SSE) through immediately (`httputil.ReverseProxy` with
  `FlushInterval = -1`), with transport timeouts that bound connection setup and
  the wait for response headers but never cut a long streaming completion.
- Logs one structured line per request (method, path, upstream status, bytes,
  caller's tailnet identity via `WhoIs`). **Never logs the token, the
  `Authorization` value, request/response bodies, or `TS_AUTHKEY`.**

## Flags

| Flag           | Default                     | Description                                                                                  |
| -------------- | --------------------------- | -------------------------------------------------------------------------------------------- |
| `--token-file` | `$CLAUDE_AUTH_PROXY_TOKEN_FILE` | Path to a file containing the subscription setup-token. **Required.** Read once at startup, trailing whitespace trimmed. Fails fast if missing or empty. Restart to rotate. |
| `--upstream`   | `https://api.anthropic.com` | Fixed upstream every request is forwarded to.                                                |
| `--hostname`   | `claude-auth-proxy`         | tsnet node hostname on the tailnet.                                                          |
| `--listen`     | `:8080`                     | Listen address used in **tsnet mode**.                                                       |
| `--state-dir`  | (none)                      | Directory for tsnet persistent state. Set this for a stable node identity across restarts.   |
| `--local-addr` | (unset)                     | If set, listen on this plain TCP address and **skip tsnet entirely** (for local/CI testing). |

## Environment

| Variable                         | Description                                                                 |
| -------------------------------- | --------------------------------------------------------------------------- |
| `TS_AUTHKEY`                     | Tailscale auth key used to join the tailnet in tsnet mode. Should be a **tagged** auth key (intended for `tag:claude-proxy`). |
| `CLAUDE_AUTH_PROXY_TOKEN_FILE`   | Fallback for `--token-file`.                                                |

The token and `TS_AUTHKEY` are **never** logged.

## Running

### Client setup (both modes)

On each client (VM), Claude Code needs two things to use the proxy:

1. `ANTHROPIC_BASE_URL` pointing at the proxy and a **dummy**
   `CLAUDE_CODE_OAUTH_TOKEN` (the proxy supplies the real one). Both are shown
   in the mode-specific examples below.
2. A `~/.claude.json` that records onboarding as **already complete**. Even with
   a valid `CLAUDE_CODE_OAUTH_TOKEN` set, Claude Code will still drop into the
   interactive login flow if it thinks you haven't onboarded. The minimum is:

   ```json
   { "hasCompletedOnboarding": true }
   ```

   > **Heads up — default model.** A real onboarding run also writes
   > subscription details into `.claude.json`, and that can pin a default model
   > that isn't what you expect (e.g. `sonnet` instead of `opus[1m]`). If a
   > session comes up on the wrong model, set it explicitly (`--model`, the
   > `ANTHROPIC_MODEL` env var, or `/model`) rather than relying on the
   > onboarding-derived default.

### tsnet mode (default — production)

Joins the tailnet as `claude-auth-proxy` and listens on `:8080` over the tailnet:

```sh
export TS_AUTHKEY=tskey-auth-...        # a tagged key for tag:claude-proxy
claude-auth-proxy \
  --token-file /run/secrets/claude-setup-token \
  --state-dir /var/lib/claude-auth-proxy
```

From a tagged client on the tailnet:

```sh
ANTHROPIC_BASE_URL=http://claude-auth-proxy:8080 \
CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-DUMMY \
claude -p "reply with exactly the single word: pong"
```

(Claude Code accepts a plaintext `http://host:port` base URL — no client-side TLS
is needed because tailnet traffic is WireGuard-encrypted — and runs happily with
a dummy `CLAUDE_CODE_OAUTH_TOKEN`.)

### local-addr mode (testing / CI)

Plain TCP socket, no tailnet, no caller identity:

```sh
claude-auth-proxy \
  --local-addr 127.0.0.1:8080 \
  --token-file ./token.txt

# In another shell:
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 \
CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-DUMMY \
claude -p "reply with exactly the single word: pong"
```

## Building

With Nix (produces a static, CGO-free binary):

```sh
nix build            # -> ./result/bin/claude-auth-proxy
```

The flake exposes `packages.${system}.default` (so nix-config can reference
`inputs.claude-auth-proxy.packages.${system}.default`) and a `devShells.default`
with Go tooling. With direnv, `cd` into the repo to load the dev shell.

Plain Go:

```sh
CGO_ENABLED=0 go build ./...
```

## Deployment

Deployed as a systemd service from nix-config, mirroring `modules/golinks.nix`:
the unit gets `TS_AUTHKEY` via an `EnvironmentFile` (sops template) and a
`--token-file` pointing at a sops-decrypted secret, plus a `--state-dir` under
`/var/lib`. The nix-config module, flake-input wiring, the VM-side environment
variables (`ANTHROPIC_BASE_URL`, dummy `CLAUDE_CODE_OAUTH_TOKEN`), and the
Tailscale ACL/tag policy live outside this repo.

## Testing

See [TESTING.md](./TESTING.md) for the full, runnable test plan (static checks,
unit tests with no tailnet, a manual integration run against real Anthropic, a
tsnet/tailnet check, and negative/hardening checks).

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](./LICENSE-APACHE) or
  <http://www.apache.org/licenses/LICENSE-2.0>)
- MIT license ([LICENSE-MIT](./LICENSE-MIT) or
  <http://opensource.org/licenses/MIT>)

at your option.

Unless you explicitly state otherwise, any contribution intentionally submitted
for inclusion in the work by you, as defined in the Apache-2.0 license, shall be
dual licensed as above, without any additional terms or conditions.
