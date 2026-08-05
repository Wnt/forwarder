# Self-hosted forwarder (reverse tunnel)

An **application-agnostic reverse tunnel** that puts apps running behind NAT onto
the public internet under a wildcard domain you control, using one small VPS and
its Caddy + Let's Encrypt — **no inbound port on the app's network**, because the
agent dials out.

[`deploy/`](deploy/) provisions that VPS from an UpCloud API token and a name:

```bash
export UPCLOUD_TOKEN=ucat_...
deploy/provision-upcloud.sh myedge tunnel.example.com
```

The first guest was the [iOS-pwa-runner](https://github.com/Wnt/iOS-pwa-runner)
(needed a stable public HTTPS origin for WebAuthn passkeys after Tailscale Funnel
/ ngrok / Cloudflare each hit a wall), but nothing here is runner-specific: point
the agent at any local HTTP/WebSocket port and a subdomain.

> Examples below use `*.lab.madekivi.fi`, the author's deployment. Substitute
> your own wildcard domain throughout.

```
                 *.lab.madekivi.fi  ──A──►  vm-control public IP
 ┌──────────────────────────────── the box (small public VPS) ─────────────────┐
 │  Caddy :443  (TLS, Let's Encrypt; on-demand per-subdomain, /ask-gated)        │
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

> **Live deployment (`vm-control`).** The forwarder runs on the author's box
> (public IP behind `*.lab.madekivi.fi`), deployed from this repo's `main` by CI.
> **Raw TCP tunnels are enabled**
> with range **`10000-19999`**. Agents dial control host
> **`tunnel.lab.madekivi.fi`**; the shared token is on the box at
> `/etc/forwarder/forwarder.env` (`FORWARDER_AGENT_TOKEN`). HTTP/WS guests pick
> any `*.lab.madekivi.fi` subdomain (e.g. `ios.lab.madekivi.fi`).

## Why this shape (and where it diverges from the original spec)

The engineering spec proposed a standalone VPS where the forwarder owns `:443` +
`:7000` and terminates TLS itself with **certmagic**. We deliberately changed two
things, so the box can also host unrelated Caddy sites:

| Spec | Here | Why |
|---|---|---|
| Forwarder owns `:443`, certmagic issues certs | **Caddy** keeps `:443`; forwarder is loopback-only behind it | Two processes can't own `:443`, so this leaves room for other sites on the same box. Caddy already manages LE certs. Drops the heavy certmagic dep → the binary builds in seconds on a 1 GB box. |
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
internal/e2e/           full server↔agent test (HTTP + 256 KB binary WS frame + raw TCP)
vendor/                 yamux + coder/websocket, vendored for a hermetic box build
```

Two small deps: `github.com/hashicorp/yamux`, `github.com/coder/websocket`.

## Get the binaries (download or build)

You need `forwarder-server` (Linux, runs on the box) and `forwarder-agent`
(macOS/Linux, runs next to your app).

**Download from CI — no Go required.** Every push/PR builds them and uploads a
`forwarder-binaries` artifact (server + agent for `darwin-arm64`, `darwin-amd64`,
`linux-amd64`). Pull the latest green `main` build with the GitHub CLI:

```bash
gh run download --repo Wnt/forwarder -n forwarder-binaries --dir bin \
  "$(gh run list --repo Wnt/forwarder --workflow CI --branch main \
       --status success --limit 1 --json databaseId --jq '.[0].databaseId')"
chmod +x bin/forwarder-agent-*        # artifacts arrive without the exec bit
```

(Or: GitHub → Actions → a green **CI** run → Artifacts → `forwarder-binaries`.)

**Build from source** — needs the Go version in `go.mod` (current stable; `deploy/install.sh` installs it from go.dev on the box); hermetic (vendored deps, no network
fetch), exactly how the box rebuilds the server during redeploy:

```bash
make all       # bin/forwarder-server + bin/forwarder-agent-{darwin-arm64,darwin-amd64,linux-amd64}
make agents    # just the agents      (make server = just the linux server)
make test      # go vet + unit + end-to-end tests (race-clean)
```

## Server (on the box) — fully automated

You don't deploy this by hand. See [`deploy/README.md`](deploy/README.md) for the
details; the short version:

- **`deploy/provision-upcloud.sh <name> <control-host>`** creates the VPS from an
  UpCloud token, injecting `cloud-init.yaml` as user_data.
- **`deploy/install.sh`** (box-side, idempotent) is the single source of truth for
  "build, install, restart": apt deps + Caddy, build from vendored source, install
  the unit + Caddyfile + nftables, `caddy validate`, restart, health gate. First
  boot and every CI deploy both run it, so a fresh box and a long-lived one
  converge on the same state.
- **`deploy/redeploy.sh`** is `git reset --hard origin/main` + `install.sh`, and is
  the forced command the CI deploy key is pinned to.

So: land a change on `main`; CI runs `go vet`/`go test`/`make all`, then SSHes to
the box and redeploys. A deploy that leaves the forwarder unhealthy exits
non-zero and fails the job.

Server config (env, all optional except the token — defaults in parentheses):

| Env | Default | Meaning |
|---|---|---|
| `FORWARDER_AGENT_TOKEN` | *(required)* | shared bearer secret agents authenticate with |
| `FORWARDER_CONTROL_HOST` | *(unset)* | FQDN agents dial; always cert-approved by `/ask` (set it!) |
| `FORWARDER_HTTP_PORT` | `7080` | loopback ingress port Caddy proxies to |
| `FORWARDER_MGMT_PORT` | `7001` | loopback `/ask` + `/status` + `/healthz` |
| `FORWARDER_CONTROL_PATH` | `/__forwarder/v1/control` | reserved control-WS path |
| `FORWARDER_MAX_CONNS_PER_TUNNEL` | `256` | per-agent concurrent public connections |
| `FORWARDER_TCP_PORT_RANGE` | *(unset = off)* | enable raw TCP tunnels on this port range, e.g. `10000-19999` (live on the box; see Raw TCP below) |
| `FORWARDER_TCP_BIND` | `0.0.0.0` | interface TCP tunnel listeners bind |
| `FORWARDER_PUBLIC_HOST` | *(unset)* | pretty host shown in a TCP tunnel's assigned address |
| `UDP_RELAY_PEER_IP` | *(unset = off)* | WireGuard address of the peer a public UDP range is DNAT'd to (see UDP relay below) |
| `UDP_RELAY_PEER_PUBKEY` | *(unset)* | that peer's WireGuard public key |
| `UDP_RELAY_PORT_RANGE` | *(unset)* | the public UDP range to relay, e.g. `54080-54130` |

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
- `--tunnel`, comma-separated for several:
  - HTTP/WS: `id:http:hostname:localport` — `hostname` is a `*.lab.madekivi.fi`
    subdomain (already pointed at the box).
  - raw TCP: `id:tcp:remoteport:localport` — needs TCP tunnels enabled on the
    server (see below); `remoteport` must fall in the server's allowed range.
- It dials **out**, so no inbound ports/port-forwarding on the guest. It
  reconnects automatically (exponential backoff, 60 s cap, jitter).

### `forwarder-agent` — CLI reference

Every flag has an env fallback, so it drops into a `.env`-driven launcher (see
[`examples/`](examples/)). All three of server/token/tunnel are required.

| Flag | Env fallback | Meaning |
|---|---|---|
| `--server <url\|host>` | `FORWARDER_SERVER` | Control endpoint: a full `ws(s)://…/__forwarder/v1/control` URL, or a bare host (expands to `wss://<host>/__forwarder/v1/control`). |
| `--token <secret>` | `FORWARDER_AGENT_TOKEN` | Shared bearer secret; must equal the server's. |
| `--tunnel <spec>[,<spec>…]` | `FORWARDER_TUNNELS` | One or more tunnels (grammar below). |
| `--insecure` | — | Skip TLS verification of the server (dev only). |

Tunnel spec grammar (colon-separated, comma-joined for several):

| Form | Example | Result |
|---|---|---|
| `id:http:hostname:localport` | `web:http:ios.lab.madekivi.fi:8080` | HTTP/WS, routed by `hostname` over `:443`; → `127.0.0.1:8080`. |
| `id:tcp:remoteport:localport` | `ssh:tcp:10022:22` | Raw TCP; server listens on public `remoteport` (must be in its range) → `127.0.0.1:22`. |

Behavior: dials **out** only (NAT-friendly); after auth it registers the tunnels
and logs the public address of each; forwards every pushed connection to
`127.0.0.1:localport`; auto-reconnects (exp backoff, 60 s cap, jitter); shuts down
on `SIGINT`/`SIGTERM`. It exits non-zero only on bad flags — a rejected tunnel is
reported per-tunnel in the logs, not fatal. Server-side knobs (the box) are the
`FORWARDER_*` env table above; the on-the-wire control protocol is defined in
[`internal/framing/framing.go`](internal/framing/framing.go).

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
- **Loopback + sandboxed.** The HTTP/control/mgmt listeners bind only `127.0.0.1`;
  the systemd unit pins sockets to loopback (`IPAddressAllow=localhost`) and runs
  read-only with no home/devices — the same backstop the rest of the lab uses.
  Enabling raw TCP tunnels lifts the loopback pin (those ports are public by
  nature); nftables then bounds exposure to the configured range.
- **The app's own auth is unchanged.** The forwarder is a transparent relay; it
  adds no auth layer. Keep the app's passkey login on.

## Status / debugging

On the box: `curl -s 127.0.0.1:7001/status` (connected agents, registered hosts +
TCP ports), `curl -s 127.0.0.1:7001/healthz`, `journalctl -u forwarder -f`. From a
guest, `forwarder-agent` logs the public address each tunnel went live at.

## Raw TCP tunnels (Phase 2) — opt-in

HTTP/WS tunnels ride Caddy on `:443`; raw TCP (e.g. `ssh` to a guest, a game
server, anything non-HTTP) can't, so it needs a **real public port** on the box.
That's a deliberate firewall-surface change, so it's **off by default** and gated
on one env var. **It is already enabled on `vm-control` (`10000-19999`)** — this
section is how it was turned on / how to change it.

To enable, set a port range on the box (`/etc/forwarder/forwarder.env`):

```
FORWARDER_TCP_PORT_RANGE=10000-19999      # the only knob; redeploy does the rest
```

On the next redeploy this:
- opens that TCP range in nftables (and **only** that range — a malformed value
  opens nothing),
- installs a systemd drop-in that lifts the forwarder's loopback IP pin (so it can
  accept public connections; nftables remains the boundary),
- makes the server accept `tcp` tunnels whose `remote_port` falls in the range.

Then a guest registers one:

```bash
forwarder-agent --server tunnel.lab.madekivi.fi --token "$TOK" \
  --tunnel ssh:tcp:10022:22        # public <box>:10022  →  guest 127.0.0.1:22
```

The forwarder opens `0.0.0.0:10022`, and every connection is spliced over the same
yamux session to the agent, which dials `127.0.0.1:22`. Ports are first-come: a
second agent requesting a bound port is rejected in its `RegisteredMsg`.

> Raw TCP carries no per-tunnel auth of its own — whatever you expose is as
> reachable as the app behind it. Keep the range tight and expose only services
> that authenticate (sshd, etc.).

## UDP relay — opt-in, and not a tunnel

Everything above is TCP: HTTP/WS rides Caddy on `:443`, raw TCP gets its own
public port. A **QUIC** app has neither option — WebTransport is UDP end to end,
and there is no way to carry it over a TCP tunnel without replacing its loss
recovery with TCP's, which is exactly what a low-latency media app was avoiding.

So UDP is handled at the kernel, not by the forwarder daemon: a public UDP port
range is DNAT'd over WireGuard to **one** peer that dials out and holds the
tunnel open. The `forwarder-agent` is not involved; nothing is multiplexed; the
datagrams stay datagrams, and loss stays loss.

```
browser ──UDP :54081──► box (dnat + snat) ──wg0──► peer 10.66.0.3:54081 ──► app
```

Set on the box (`/etc/forwarder/forwarder.env`), then redeploy:

```
UDP_RELAY_PEER_IP=10.66.0.3
UDP_RELAY_PEER_PUBKEY=<the peer's wg pubkey>
UDP_RELAY_PORT_RANGE=54080-54130
WG_EDGE_PRIVKEY=<wg genkey>        # shared with the site tunnel if both are on
```

That renders a second `[Peer]` into `wg0.conf`, appends the `forwarder_nat`
table, opens the range in nftables, and installs the one `ip_forward` drop-in on
the box. **The port is never translated** — public `:N` is peer `:N` — so the app
needs no port map and its signaling only has to advertise a different host.
Source addresses are rewritten to `WG_EDGE_ADDR`, because the peer has no route
back to an arbitrary internet client; the peer sees every client as the tunnel
address with a distinct source port, which is all a per-connection protocol like
QUIC needs to keep its 4-tuples apart.

The peer side is an ordinary `wg-quick` client with `PersistentKeepalive` — see
[`examples/wg-peer.conf`](examples/wg-peer.conf).

> Like raw TCP, the relay adds **no auth of its own**. It is a hole in the
> firewall to one host's ports. Only point it at something that authenticates its
> own sessions, keep the range tight, and remember that the forward chain's
> default is still drop — the relay range to the relay peer is the only thing
> that crosses it.

## Phase 3: WebRTC / coturn (not here)

A guest that wants H.264-over-WebRTC instead of the default JPEG-over-WS would run
coturn alongside its app; that's guest-side and independent of this tunnel, so
it's intentionally out of scope for the forwarder.

## License

MIT — see [LICENSE](LICENSE).

The two vendored dependencies are compatible: `hashicorp/yamux` is MPL-2.0
(file-level copyleft, no obligation on this code) and `coder/websocket` is ISC.
