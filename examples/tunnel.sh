#!/usr/bin/env bash
# Example launcher for the forwarder agent on a guest machine (e.g. a Mac running
# the iOS-pwa-runner). Drop a copy next to your app, point AGENT_BIN at the right
# prebuilt binary (or `make agents` output), and fill in a .env. This is the
# generic replacement for a Tailscale/ngrok `tunnel.sh`.
#
#   ./tunnel.sh up      # start the agent in the background
#   ./tunnel.sh down    # stop it
#   ./tunnel.sh run     # run in the foreground (for launchd/systemd/debug)
#   ./tunnel.sh status  # is it up?
#
# Config (env or a .env beside this script):
#   FORWARDER_SERVER     wss URL or host of the lab box's control endpoint
#                        (e.g. tunnel.lab.madekivi.fi)
#   FORWARDER_AGENT_TOKEN  shared secret (read off the box's lab-control.env)
#   FORWARDER_TUNNELS    id:http:hostname:localport[,...]
#                        (e.g. web:http:ios.lab.madekivi.fi:8080)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "${HERE}/.env" ] && { set -a; . "${HERE}/.env"; set +a; }

AGENT_BIN="${AGENT_BIN:-${HERE}/forwarder-agent}"
PIDFILE="${PIDFILE:-/tmp/forwarder-agent.pid}"
: "${FORWARDER_SERVER:?set FORWARDER_SERVER (e.g. tunnel.lab.madekivi.fi)}"
: "${FORWARDER_AGENT_TOKEN:?set FORWARDER_AGENT_TOKEN}"
: "${FORWARDER_TUNNELS:?set FORWARDER_TUNNELS (id:http:hostname:localport)}"

run() {
  exec "${AGENT_BIN}" \
    --server "${FORWARDER_SERVER}" \
    --token  "${FORWARDER_AGENT_TOKEN}" \
    --tunnel "${FORWARDER_TUNNELS}"
}

case "${1:-}" in
  run) run ;;
  up)
    if [ -f "${PIDFILE}" ] && kill -0 "$(cat "${PIDFILE}")" 2>/dev/null; then
      echo "already running (PID $(cat "${PIDFILE}"))"; exit 0
    fi
    ( run ) >/tmp/forwarder-agent.log 2>&1 &
    echo $! >"${PIDFILE}"
    echo "forwarder agent started (PID $!), logging to /tmp/forwarder-agent.log"
    ;;
  down)
    [ -f "${PIDFILE}" ] && kill "$(cat "${PIDFILE}")" 2>/dev/null || true
    rm -f "${PIDFILE}"
    echo "stopped"
    ;;
  status)
    if [ -f "${PIDFILE}" ] && kill -0 "$(cat "${PIDFILE}")" 2>/dev/null; then
      echo "up (PID $(cat "${PIDFILE}"))"
    else
      echo "down"; exit 1
    fi
    ;;
  *) echo "usage: $0 {up|down|run|status}" >&2; exit 2 ;;
esac
