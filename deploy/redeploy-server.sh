#!/usr/bin/env bash
# Rebuild only the daemon. Network services and unrelated apps keep their config.
set -euo pipefail
REPO_DIR="${REPO_DIR:-/opt/forwarder}"
cd "$REPO_DIR"
export PATH="/usr/local/go/bin:$PATH"
GOTOOLCHAIN=local go build -mod=vendor -o /usr/local/bin/forwarder-server.new ./cmd/forwarder-server
mv /usr/local/bin/forwarder-server.new /usr/local/bin/forwarder-server
install -m 644 deploy/systemd/forwarder.service /etc/systemd/system/forwarder.service
systemctl daemon-reload
systemctl restart forwarder
for attempt in $(seq 1 20); do
  if curl -fsS -m 1 http://127.0.0.1:7001/healthz; then
    exit 0
  fi
  sleep 0.2
done
echo 'forwarder did not become healthy' >&2
exit 1
