#!/bin/bash
# Encrypt a credential read from stdin into /etc/credstore.encrypted, as
# root, without it ever touching argv, a terminal echo or a plain file:
#
#   ... | sudo ./deploy/put-credential.sh anthropic-workers <sha256 of the value>
#
# The expected sha256 is computed by the sender from the same bytes; the
# script compares it and prints only "match" or "mismatch".
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
[ $# -eq 2 ] || { echo "usage: $0 <anthropic-workers|github-app-workers> <expected sha256>" >&2; exit 1; }
case "$1" in anthropic-workers|github-app-workers) ;; *) echo "unknown credential $1" >&2; exit 1 ;; esac
NAME=$1
WANT=$2
OUT=/etc/credstore.encrypted/$NAME.cred
umask 077
install -d -m 0700 /etc/credstore.encrypted
TMP=$(mktemp -d /run/claudew-cred.XXXXXX)
trap 'rm -rf "$TMP"' EXIT
cat > "$TMP/value"
GOT=$(sha256sum "$TMP/value" | awk '{print $1}')
if [ "$GOT" != "$WANT" ]; then
  echo "mismatch: nothing written" >&2
  exit 1
fi
systemd-creds encrypt --with-key=host --name="$NAME" "$TMP/value" "$OUT"
# Read it back the way systemd will and compare.
BACK=$(systemd-creds decrypt --name="$NAME" "$OUT" - | sha256sum | awk '{print $1}')
[ "$BACK" = "$WANT" ] || { echo "mismatch after encryption" >&2; rm -f "$OUT"; exit 1; }
echo "match: $OUT"
