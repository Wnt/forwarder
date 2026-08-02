#!/usr/bin/env bash
# Dead-man's switch for an offsite backup, run on the edge box.
#
# WHY IT IS NOT ON THE MACHINE BEING BACKED UP:
# a systemd OnFailure= handler only fires when a job runs and fails. It cannot
# fire when the job never runs — host powered off, VM dead, timer disabled,
# machine stolen or burned. In those cases silence is indistinguishable from
# success, and silence is exactly what a backup failure looks like. So the check
# has to live somewhere that survives the thing it watches.
#
# WHY IT DOES NOT NEED THE RESTIC PASSWORD:
# restic writes one small file per snapshot under snapshots/, and the file's
# modification time is the snapshot time. Listing that directory answers "did a
# backup land recently?" without decrypting anything. So this box holds Drive
# READ/WRITE access but NO ability to read your backups' contents — a compromise
# here does not disclose the database, the secrets or the passkey store.
#
# It also checks the real artifact rather than a heartbeat file: a heartbeat can
# be written by a script whose upload silently failed; a snapshot object cannot.
set -uo pipefail

RCLONE_CONFIG_FILE="${RCLONE_CONFIG_FILE:-/etc/forwarder/escrow/rclone.conf}"
ALERT_ENV="${ALERT_ENV:-/etc/forwarder/escrow/alert.env}"   # ROUTINE_URL + ROUTINE_TOKEN
REMOTE="${REMOTE:-gdrive:greenhouse-backups}"
MAX_AGE_HOURS="${MAX_AGE_HOURS:-26}"   # daily backup + 20m jitter + slack

log() { logger -t backup-watchdog "$*"; echo "$*"; }

alert() {
  log "ALERT: $1"
  # shellcheck disable=SC1090
  [ -r "$ALERT_ENV" ] && { set -a; . "$ALERT_ENV"; set +a; }
  if [ -n "${ROUTINE_URL:-}" ] && [ -n "${ROUTINE_TOKEN:-}" ]; then
    # Payload/headers must match server/lib/routine-trigger.js or the endpoint 400s.
    curl -fsS -m 30 -X POST "$ROUTINE_URL" \
      -H "Authorization: Bearer ${ROUTINE_TOKEN}" \
      -H 'anthropic-version: 2023-06-01' \
      -H 'anthropic-beta: experimental-cc-routine-2026-04-01' \
      -H 'Content-Type: application/json' \
      -d "$(python3 -c "import json,sys; print(json.dumps({'text': sys.argv[1]}))" \
            "BACKUP WATCHDOG (edge box): $1")" >/dev/null \
      && log "incident routine fired" || log "FAILED to fire incident routine"
  else
    log "no alert credentials — this alert exists only in the journal"
  fi
}

[ -r "$RCLONE_CONFIG_FILE" ] || { log "no rclone config at $RCLONE_CONFIG_FILE"; exit 1; }
export RCLONE_CONFIG="$RCLONE_CONFIG_FILE"

# Failing to list at all is itself the alert: Drive revoked the token, the OAuth
# client was deleted, or the folder is gone.
# lsjson, not lsl: lsl prints times in the machine's LOCAL zone with no offset,
# so a timezone difference would silently skew freshness by hours in either
# direction. lsjson emits RFC3339 with an explicit offset.
if ! listing=$(rclone lsjson "${REMOTE}/snapshots/" 2>&1); then
  alert "cannot list the backup repository: $(printf '%s' "$listing" | tail -2 | tr '\n' ' ' | cut -c1-300)"
  exit 1
fi

age_h=$(printf '%s' "$listing" | python3 -c "
import sys, json, datetime
try:
    entries = json.load(sys.stdin)
except Exception:
    print(-1); raise SystemExit
times = []
for e in entries:
    mt = e.get('ModTime')
    if not mt:
        continue
    try:
        times.append(datetime.datetime.fromisoformat(mt.replace('Z', '+00:00')))
    except ValueError:
        pass
if not times:
    print(-1)
else:
    now = datetime.datetime.now(datetime.timezone.utc)
    print(int((now - max(times)).total_seconds() // 3600))
")

if [ -z "$age_h" ] || [ "$age_h" = "-1" ]; then
  alert "the backup repository contains NO usable snapshots"
  exit 1
fi

if [ "$age_h" -gt "$MAX_AGE_HOURS" ]; then
  alert "newest backup is ${age_h}h old (threshold ${MAX_AGE_HOURS}h) — the home host may be down, or backups are failing silently"
  exit 1
fi

log "ok: newest backup is ${age_h}h old"
