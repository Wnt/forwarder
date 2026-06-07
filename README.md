# Self-hosted forwarder (reverse tunnel)

An **application-agnostic reverse tunnel** that puts apps running behind NAT onto
the public internet at `*.lab.madekivi.fi`, **reusing the lab box** (`vm-control`)
and its existing Caddy + Let's Encrypt — no extra VPS, no new firewall port.

The first guest is the [iOS-pwa-runner](https://github.com/Wnt/iOS-pwa-runner)
(needed a stable public HTTPS origin for WebAuthn passkeys after Tailscale Funnel
/ ngrok / Cloudflare each hit a wall), but nothing here is runner-specific: point
the agent at any local HTTP/WebSocket port and a `*.lab.madekivi.fi` subdomain.

```
                 *.lab.madekivi.fi  ──A──►  vm-control public IP
 ┌──────────────────────────────── vm-control (the lab box) ───────────────────┐
 │  Caddy :443  (TLS, Let's Encrypt; on-demand per-subdomain, /ask-gated)        │
 │    ├─ {$LAB_HOSTNAME}            → lab console (unchanged)                     │
 │    └─ * (catch-all, on_demand)   → 127.0.0.1:7080  forwarder-server           │
 │                                      ├─ control WS  (agents dial in)          │
 │                                      ├─ public ingress (route by Host)        │
 │                                      └─ :7001 /ask /status (loopback mgmt)     │
 └──────────────────────────────────────────────────────────────────────────────┘
            ▲  one persistent wss + yamux, dialed OUT (NAT-friendly)
 ┌──────────┼─ guest machine (e.g. a Mac behind a home router) ─────────────────┐
 │  forwarder-agent  ──dials──►  wss://tunnel.lab.madekivi.fi/__forwarder/v1/...  │
 │    on each pushed stream → dial 127.0.0.1:<localport> → io.Copy both ways      │
 │  the app (e.g. iOS-pwa-runner on :8080)  ← unchanged, unaware of the tunnel    │
 └──────────────────────────────────────────────────────────────────────────────┘
```

## Why this shape (and where it diverges from the original spec)

The engineering spec proposed a standalone VPS where the forwarder owns `:443` +
`:7000` and terminates TLS itself with **certmagic**. We deliberately changed two
things to **reuse the lab box**, which already runs Caddy on `:80/:443`:

| Spec | Here | Why |
|---|---|---|
| Forwarder owns `:443`, certmagic issues certs | **Caddy** keeps `:443`; forwarder is loopback-only behind it | Two processes can't own `:443`; Caddy already has a valid LE cert + ACME account. Drops the heavy certmagic dep → the binary builds in seconds on the 1 GB box. |
| Agent dials a dedicated TLS `:7000` | Agent dials **wss through Caddy on `:443`** | No new firewall hole (nftables stays `22/80/443`); reuses the LE cert (agent trusts system roots); works through restrictive guest networks. yamux just rides a WebSocket (`coder/websocket` `NetConn`). |

Everything else follows the spec: yamux multiplexing, newline-JSON control
protocol, per-host routing, transparent WebSocket passthrough (no frame
buffering), on-demand TLS gated so the forwarder is **never an open relay**.

## Layout

```
cmd/forwarder-server/   thin main: env config + the two loopback listeners
cmd/forwarder-agent/    thin main: flags/env + the reconnect loop
internal/framing/       wire protocol: control messages + connect header + yamux cfg
internal/server/        registry, control handshake, HTTP/WS proxy, /ask guard
internal/agent/         dial → auth → register → forward data streams
internal/e2e/           full server↔agent test (HTTP + 256 KB binary WS frame)
vendor/                 yamux + coder/websocket, vendored for a hermetic box build
```

Two small deps: `github.com/hashicorp/yamux`, `github.com/coder/websocket`.

## Build & test

```bash
cd lab/forwarder
make test          # go vet + unit + end-to-end tests (race-clean)
make server        # bin/forwarder-server          (linux/amd64 — what the box runs)
make agents        # bin/forwarder-agent-{darwin-arm64,darwin-amd64,linux-amd64}
```

The build is hermetic — `GOTOOLCHAIN=local` + vendored deps, so it fetches
nothing. That's exactly how `vm-control` rebuilds the server during redeploy.

## Server (on the lab box) — fully automated

You don't deploy this by hand. The forwarder is part of the lab's
deploy-from-`main` loop:

- **`deploy/setup.sh`** (one-time provisioning) installs `golang-go` and seeds
  `FORWARDER_AGENT_TOKEN` (random) + `FORWARDER_CONTROL_HOST` into the env file.
- **`deploy/redeploy.sh`** (every push to `main`) builds the server from vendored
  source, installs the `forwarder.service` unit + the Caddy `conf.d/forwarder.caddy`
  catch-all site, then restarts both. A `caddy validate` gate fails the deploy if
  the config is bad.

So: land a change to `lab/forwarder/**` (or `lab/lab-control/deploy/**`) on
`main`; CI runs `go test`, then SSHes to the box and redeploys. To bring up the
forwarder on the **existing** live box for the first time, re-run `setup.sh` once
(it provisions Go + the token); thereafter redeploys are automatic.

Server config (env, all optional except the token — defaults in parentheses):

| Env | Default | Meaning |
|---|---|---|
| `FORWARDER_AGENT_TOKEN` | *(required)* | shared bearer secret agents authenticate with |
| `FORWARDER_CONTROL_HOST` | *(unset)* | FQDN agents dial; always cert-approved by `/ask` (set it!) |
| `FORWARDER_HTTP_PORT` | `7080` | loopback ingress port Caddy proxies to |
| `FORWARDER_MGMT_PORT` | `7001` | loopback `/ask` + `/status` + `/healthz` |
| `FORWARDER_CONTROL_PATH` | `/__forwarder/v1/control` | reserved control-WS path |
| `FORWARDER_MAX_CONNS_PER_TUNNEL` | `256` | per-agent concurrent public connections |

## Agent (next to your app) — e.g. the iOS-pwa-runner

On the guest machine, build/copy the right `forwarder-agent` binary and run it
alongside the app. See [`examples/`](examples/) for a `tunnel.sh` launcher, a
macOS LaunchAgent, and an env template.

```bash
forwarder-agent \
  --server wss://tunnel.lab.madekivi.fi/__forwarder/v1/control \
  --token  "$FORWARDER_AGENT_TOKEN" \
  --tunnel web:http:ios.lab.madekivi.fi:8080
```

- `--server` accepts a full `ws(s)://` URL **or** a bare host (then it defaults to
  `wss://<host>/__forwarder/v1/control`).
- `--tunnel id:http:hostname:localport`, comma-separated for several. The
  `hostname` must be a `*.lab.madekivi.fi` subdomain (already pointed at the box).
- It dials **out**, so no inbound ports/port-forwarding on the guest. It
  reconnects automatically (exponential backoff, 60 s cap, jitter).

The app itself is unchanged and unaware of the tunnel. For the iOS-pwa-runner
specifically, set in **its** `.env` so passkeys bind to the public origin:

```
RPID=ios.lab.madekivi.fi
ORIGIN=https://ios.lab.madekivi.fi
AUTH_SECURE_COOKIES=true
```

(The hostname is the WebAuthn anchor — keep it stable or you invalidate enrolled
passkeys. Never run the app with `RPID=localhost` behind the tunnel.)

## Security model

- **Not an open relay.** Caddy's on-demand TLS only mints a cert if the
  forwarder's `/ask` approves the SNI — i.e. the control host or a hostname a
  *live* agent registered. The HTTP router independently `502`s unknown Hosts.
- **Token auth.** Agents authenticate with `FORWARDER_AGENT_TOKEN`
  (constant-time compared); repeated failures from an IP trip a short ban.
- **Loopback + sandboxed.** The server binds only `127.0.0.1`; its systemd unit
  pins sockets to loopback (`IPAddressAllow=localhost`) and runs read-only with
  no home/devices — the same backstop the rest of the lab uses.
- **The app's own auth is unchanged.** The forwarder is a transparent relay; it
  adds no auth layer. Keep the app's passkey login on.

## Status / debugging

On the box: `curl -s 127.0.0.1:7001/status` (connected agents + registered hosts),
`curl -s 127.0.0.1:7001/healthz`, `journalctl -u forwarder -f`. From a guest,
`forwarder-agent` logs the public URL each tunnel went live at.

## Phase 2 / 3 (not enabled)

- **Raw TCP tunnels** (`proto:"tcp"`): the protocol carries them, but they can't
  ride Caddy's HTTP front door, so they'd need a dedicated listen port + an
  nftables hole — out of scope for "reuse the box, no new firewall surface". The
  server currently rejects `tcp` tunnels with a clear per-tunnel error.
- **WebRTC / coturn**: a guest that wants H.264-over-WebRTC (not the default
  JPEG-over-WS) would run coturn; that's guest-side and independent of this
  tunnel. Not deployed here.
