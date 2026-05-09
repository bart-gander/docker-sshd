#!/usr/bin/env sh
# Live SCP verification script.
# Run this against a running kube-sshd or docker-sshd instance.
set -eu

PORT="${PORT:-2232}"
KH="${KH:-/tmp/sshd_known_hosts}"
HOST="${HOST:-127.0.0.1}"
SRC="${SRC:-/tmp/scp-test-src.txt}"
DST_REMOTE="${DST_REMOTE:-/tmp/scp-test-remote.txt}"
DST_LOCAL="${DST_LOCAL:-/tmp/scp-test-down.txt}"

# Adjust SSH_TARGET_ENC to match your deployment's namespace/pod/service encoding.
SSH_TARGET_ENC="${SSH_TARGET_ENC:-namespace%2Fpod%2Fservice}"
SSH_USER="${SSH_USER:-scp-user}"

printf 'live-scp-test %s\n' "$(date +%s)" > "$SRC"

echo '--- upload ---'
scp -O -v \
  -o StrictHostKeyChecking=accept-new \
  -o UserKnownHostsFile="$KH" \
  -o PreferredAuthentications=none \
  -o PubkeyAuthentication=no \
  -P "$PORT" \
  "$SRC" \
  "scp://${SSH_TARGET_ENC}@${HOST}//$DST_REMOTE"

echo '--- download ---'
scp -O -v \
  -o StrictHostKeyChecking=accept-new \
  -o UserKnownHostsFile="$KH" \
  -o PreferredAuthentications=none \
  -o PubkeyAuthentication=no \
  -P "$PORT" \
  "scp://${SSH_TARGET_ENC}@${HOST}//$DST_REMOTE" \
  "$DST_LOCAL"

echo '--- compare ---'
if cmp -s "$SRC" "$DST_LOCAL"; then
  echo 'LIVE_SCP_OK'
else
  echo 'LIVE_SCP_MISMATCH'
  diff -u "$SRC" "$DST_LOCAL" || true
  exit 1
fi