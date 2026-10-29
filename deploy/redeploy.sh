#!/usr/bin/env bash
# The forced-command entry point for CI auto-deploy: sync to origin/main, then
# hand off to install.sh.
#
# The CI deploy key's authorized_keys entry pins command="/opt/forwarder/deploy/
# redeploy.sh", so that key can do nothing else on this box -- it cannot open a
# shell, run an arbitrary command, or forward a port. Whatever the client asks
# for lands in SSH_ORIGINAL_COMMAND and is ignored.
#
# install.sh's health gate makes a bad deploy exit non-zero, which fails the CI
# job rather than silently leaving the box broken.
set -euo pipefail

REPO_DIR="${REPO_DIR:-/opt/forwarder}"
BRANCH="${BRANCH:-main}"

cd "$REPO_DIR"
previous=$(git rev-parse HEAD)
echo "== sync ${BRANCH}"
git fetch --quiet origin "$BRANCH"
git reset --hard "origin/${BRANCH}"
git --no-pager log --oneline -1

# Application-only changes need no package, firewall, Caddy, HAProxy or
# WireGuard changes. Restart just the forwarder; agents reconnect themselves.
if ! git diff --name-only "$previous" HEAD | grep -qvE \
  '^(internal/|cmd/|tools/tunnel-load/|README\.md$|deploy/(hosts\.json|server\.env|systemd/forwarder\.service|redeploy(-server)?\.sh|README\.md)$)'; then
  exec "${REPO_DIR}/deploy/redeploy-server.sh"
fi

exec "${REPO_DIR}/deploy/install.sh"
