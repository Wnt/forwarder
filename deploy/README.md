# Deploying the forwarder box

The box-side half runs on any small public Debian machine. Everything here is
idempotent, and `install.sh` is the single source of truth for "build, install,
restart" — first boot and every later CI deploy both end up in it, so a fresh box
and a long-lived one converge on the same state.

| File | Role |
|---|---|
| `provision-upcloud.sh` | **Laptop-side.** Creates the server on UpCloud from an API token and a name, injecting `cloud-init.yaml` as user_data. |
| `cloud-init.yaml` | First-boot user_data. Thin: seeds the env file, clones the repo, runs `install.sh`. |
| `install.sh` | **Box-side, idempotent.** apt deps + Caddy, builds `forwarder-server` from vendored source, installs the unit + Caddyfile + nftables, validates, restarts, health-gates. |
| `redeploy.sh` | **Box-side.** `git reset --hard origin/main` then `install.sh`. The forced command the CI deploy key is pinned to. |
| `Caddyfile` | The whole Caddy config for a dedicated box: gated on-demand TLS + the catch-all site proxying to the forwarder. |
| `nftables.conf` | Host firewall. Inbound dropped by default; only 22/80/443 plus the raw-TCP tunnel range when enabled. |
| `systemd/forwarder.service` | The server unit — loopback-pinned and hardened. |
| `env.example` | Template for `/etc/forwarder/forwarder.env` (root, 0600). **No secrets in the repo.** |

## From zero

```bash
export UPCLOUD_TOKEN=ucat_...
deploy/provision-upcloud.sh myedge tunnel.example.com
```

Then point DNS at the printed IP — both the control host and a wildcard for
whatever you plan to publish (`*.tunnel.example.com`). Until DNS resolves, Caddy
cannot complete an ACME challenge and no agent can dial in.

## Publishing an app

Run the agent next to the app. It dials **out**, so the app's network needs no
inbound ports and no port forwarding:

```bash
forwarder-agent \
  --server tunnel.example.com \
  --token  "$FORWARDER_AGENT_TOKEN" \
  --tunnel web:http:myapp.tunnel.example.com:8080
```

The first request to `myapp.tunnel.example.com` makes Caddy mint a certificate —
but only because the forwarder's `/ask` confirms a live agent registered that
hostname. An unregistered SNI is refused, so the catch-all is never an open relay
and cannot be used to burn your Let's Encrypt rate limit.

## CI auto-deploy

Pushes to `main` run the tests and then SSH to the box. Three repo secrets:

| Secret | Value |
|---|---|
| `DEPLOY_HOST` | the box's IP or FQDN |
| `DEPLOY_SSH_KEY` | private half of a dedicated ed25519 key |
| `DEPLOY_KNOWN_HOSTS` | the box's host key (`ssh-keyscan <host>`) |

On the box, pin that key to the redeploy script so it can do nothing else — not
open a shell, not run another command, not forward a port:

```
command="/opt/forwarder/deploy/redeploy.sh",no-agent-forwarding,no-port-forwarding,no-pty,no-user-rc ssh-ed25519 AAAA... forwarder-ci
```

`install.sh` health-gates at the end, so a deploy that leaves the forwarder
unhealthy exits non-zero and fails the CI job rather than quietly breaking the
box.

## Adding the forwarder to a box that already runs Caddy

Do **not** install `deploy/Caddyfile` — it is a whole config, and it would
replace the sites already there. Instead copy its two blocks into the existing
Caddyfile: the global `on_demand_tls` block merges, and the catch-all `https://`
site loses to every named site because Caddy matches most-specific host first.
