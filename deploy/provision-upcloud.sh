#!/usr/bin/env bash
# Create a forwarder box on UpCloud from nothing but an API token and a name.
#
#   export UPCLOUD_TOKEN=ucat_...
#   deploy/provision-upcloud.sh myedge tunnel.example.com
#
# Everything else has a default: the STARTER-1xCPU-1GB plan (the ~3 EUR/mo box),
# Debian 13, your default SSH public key. The box comes up with Caddy + the
# forwarder already running, because cloud-init clones this repo and runs
# deploy/install.sh -- the same script CI runs on every deploy.
#
# The agent token is generated HERE and printed once at the end. It is injected
# through cloud-init user_data, never committed.
#
# upctl reads UPCLOUD_TOKEN from the environment (>= v3.20). If upctl is not
# installed, this script fetches the release binary into a temp dir rather than
# installing it system-wide.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

NAME="${1:-}"
CONTROL_HOST="${2:-${FORWARDER_CONTROL_HOST:-}}"
PLAN="${PLAN:-STARTER-1xCPU-1GB}"
ZONE="${ZONE:-fi-hel1}"
OS="${OS:-Debian GNU/Linux 13 (Trixie)}"
SSH_PUBKEY="${SSH_PUBKEY:-$HOME/.ssh/id_ed25519.pub}"
REPO_URL="${REPO_URL:-https://github.com/Wnt/forwarder.git}"
TCP_RANGE="${FORWARDER_TCP_PORT_RANGE:-}"

usage() {
  cat >&2 <<EOF
usage: UPCLOUD_TOKEN=... $0 <name> <control-host-fqdn>

  <name>              server + hostname on UpCloud, e.g. myedge
  <control-host>      FQDN agents dial, e.g. tunnel.example.com
                      Point this (and ideally a *.wildcard beside it) at the
                      printed IP; the box cannot serve TLS for it until then.

env overrides: PLAN ZONE OS SSH_PUBKEY REPO_URL FORWARDER_TCP_PORT_RANGE
EOF
  exit 2
}

[ -n "$NAME" ] && [ -n "$CONTROL_HOST" ] || usage
[ -n "${UPCLOUD_TOKEN:-}" ] || { echo "UPCLOUD_TOKEN is not set" >&2; exit 2; }
[ -r "$SSH_PUBKEY" ] || { echo "no SSH public key at $SSH_PUBKEY" >&2; exit 2; }

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

# --- upctl -----------------------------------------------------------------
if command -v upctl >/dev/null; then
  UPCTL=upctl
else
  say "Fetching upctl"
  ver=$(curl -fsS https://api.github.com/repos/UpCloudLtd/upcloud-cli/releases/latest \
        | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -1)
  [ -n "$ver" ] || { echo "could not determine latest upctl version" >&2; exit 1; }
  curl -fsSL -o "$TMP/upctl.tgz" \
    "https://github.com/UpCloudLtd/upcloud-cli/releases/download/v${ver}/upcloud-cli_${ver}_linux_x86_64.tar.gz"
  tar xzf "$TMP/upctl.tgz" -C "$TMP"
  UPCTL="$TMP/upctl"
  chmod +x "$UPCTL"
  echo "using upctl ${ver} (temporary, not installed system-wide)"
fi

"$UPCTL" account show >/dev/null || { echo "UPCLOUD_TOKEN rejected" >&2; exit 1; }

# --- user_data -------------------------------------------------------------
say "Rendering cloud-init"
AGENT_TOKEN=$(openssl rand -hex 32)
extra=""
[ -n "$TCP_RANGE" ] && extra="FORWARDER_TCP_PORT_RANGE=${TCP_RANGE}"
sed -e "s|@AGENT_TOKEN@|${AGENT_TOKEN}|" \
    -e "s|@CONTROL_HOST@|${CONTROL_HOST}|" \
    -e "s|@EXTRA_ENV@|${extra}|" \
    -e "s|@REPO_URL@|${REPO_URL}|" \
    "$HERE/cloud-init.yaml" > "$TMP/user-data.yaml"

# --- create ----------------------------------------------------------------
say "Creating ${NAME} (${PLAN}, ${ZONE})"
"$UPCTL" server create \
  --hostname "$NAME" \
  --title "$NAME" \
  --plan "$PLAN" \
  --zone "$ZONE" \
  --os "$OS" \
  --enable-metadata \
  --ssh-keys "$SSH_PUBKEY" \
  --user-data "$TMP/user-data.yaml" \
  --network type=public,family=IPv4 \
  --wait

IP=$("$UPCTL" server show "$NAME" -o json | python3 -c "
import json, sys
d = json.load(sys.stdin)
for iface in d['networking']['interfaces']:
    if iface['type'] != 'public':
        continue
    for addr in iface['ip_addresses']:
        if addr['family'] == 'IPv4':
            print(addr['address']); raise SystemExit
raise SystemExit('no public IPv4 found')
")

say "Done"
cat <<EOF
  server:        ${NAME}
  public IPv4:   ${IP}
  control host:  ${CONTROL_HOST}
  agent token:   ${AGENT_TOKEN}
                 (also in /etc/forwarder/forwarder.env on the box)

Next:
  1. DNS: point ${CONTROL_HOST} -> ${IP}, plus a wildcard for the hostnames you
     intend to publish, e.g. *.tunnel.example.com -> ${IP}. Until DNS resolves,
     Caddy cannot complete an ACME challenge and agents cannot dial in.
  2. Wait for cloud-init: ssh root@${IP} 'cloud-init status --wait'
  3. Run an agent next to your app:
       forwarder-agent --server ${CONTROL_HOST} \\
         --token ${AGENT_TOKEN} \\
         --tunnel web:http:myapp.tunnel.example.com:8080
  4. For CI auto-deploy, add a forced-command deploy key -- see deploy/README.md.
EOF
