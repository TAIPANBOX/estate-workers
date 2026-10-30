#!/bin/bash
# Install estate workers on a Debian 13 host. Run as root from a directory
# holding the claudew and secretscan binaries (built for linux/amd64) and
# this deploy/ tree:
#
#   sudo ./deploy/install.sh /path/to/claudew /path/to/secretscan
#
# Idempotent. Enables only the push service socket; the drain timer stays
# off until the security checks and the pilot have passed. Credentials are
# added separately with deploy/put-credential.sh.
set -euo pipefail

CLAUDE_VERSION="2.1.308-1"
CLAUDE_KEY_FPR="31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE"
GO_VERSION="1.27.3"
GO_SHA256="65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40"

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
[ $# -eq 2 ] || { echo "usage: $0 <claudew binary> <secretscan binary>" >&2; exit 1; }
BIN_CLAUDEW=$1
BIN_SECRETSCAN=$2
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date -u +%Y%m%d-%H%M%S)
BACKUP=/var/backups/claudew-install-$STAMP

echo "== snapshot to $BACKUP"
install -d -m 0700 "$BACKUP"
{
  echo "# before claudew install, $STAMP UTC"
  dpkg -l | awk '/^ii/ {print $2, $3}'
  echo "# users"; getent passwd claudew claudew-gh || true
  echo "# groups"; getent group claudew claudew-gh || true
  echo "# units"; systemctl list-unit-files 'claudew*' --no-pager || true
  echo "# apt sources"; ls /etc/apt/sources.list.d/
  echo "# go"; ls -d /usr/local/go 2>/dev/null || echo "no /usr/local/go"
} > "$BACKUP/before.txt"

echo "== Anthropic apt repository, key checked by fingerprint"
install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://downloads.claude.ai/keys/claude-code.asc -o "$BACKUP/claude-code.asc"
# A throwaway keyring home, so root gets no ~/.gnupg from the check.
GNUPGHOME=$(mktemp -d)
export GNUPGHOME
fpr=$(gpg --show-keys --with-colons "$BACKUP/claude-code.asc" | awk -F: '/^fpr/ {print $10; exit}')
rm -rf "$GNUPGHOME"
unset GNUPGHOME
[ "$fpr" = "$CLAUDE_KEY_FPR" ] || { echo "key fingerprint $fpr is not $CLAUDE_KEY_FPR" >&2; exit 1; }
install -m 0644 "$BACKUP/claude-code.asc" /etc/apt/keyrings/claude-code.asc
echo "deb [signed-by=/etc/apt/keyrings/claude-code.asc] https://downloads.claude.ai/claude-code/apt/stable stable main" \
  > /etc/apt/sources.list.d/claude-code.list
apt-get update -q
# make: the templates' gates start it (dossier-bump runs `make gates`).
apt-get install -y -q "claude-code=$CLAUDE_VERSION" bubblewrap socat make

echo "== Go $GO_VERSION"
if [ ! -x /usr/local/go/bin/go ] || ! /usr/local/go/bin/go version | grep -q "go$GO_VERSION "; then
  curl -fsSL "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz" -o "$BACKUP/go.tgz"
  echo "$GO_SHA256  $BACKUP/go.tgz" | sha256sum -c -
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$BACKUP/go.tgz"
  rm -f "$BACKUP/go.tgz"
fi

echo "== users"
getent group claudew >/dev/null || groupadd --system claudew
getent passwd claudew >/dev/null || useradd --system --gid claudew --home-dir /var/lib/claudew --no-create-home --shell /usr/sbin/nologin claudew
getent group claudew-gh >/dev/null || groupadd --system claudew-gh
getent passwd claudew-gh >/dev/null || useradd --system --gid claudew-gh --groups claudew --home-dir /var/lib/claudew-gh --no-create-home --shell /usr/sbin/nologin claudew-gh

echo "== directories"
install -d -m 0750 -o claudew -g claudew /var/lib/claudew
install -d -m 0750 -o claudew -g claudew /var/lib/claudew/runs
install -d -m 0750 -o claudew-gh -g claudew /var/lib/claudew-gh
install -d -m 0700 -o claudew-gh -g claudew-gh /var/lib/claudew-gh/cache /var/lib/claudew-gh/home
# setgid, so each bundle gets group claudew and the drain service can read it.
install -d -m 2750 -o claudew-gh -g claudew /var/lib/claudew-gh/export
install -d -m 0755 -o root -g root /etc/claudew /opt/claudew/bin /etc/claude-code
install -m 0644 -o claudew -g claudew /dev/null /var/lib/prometheus/node-exporter/claudew.prom

echo "== binaries"
install -m 0755 -o root -g root "$BIN_CLAUDEW" /opt/claudew/bin/claudew
install -m 0755 -o root -g root "$BIN_SECRETSCAN" /opt/claudew/bin/secretscan
sha256sum /opt/claudew/bin/claudew /opt/claudew/bin/secretscan | tee "$BACKUP/binaries.sha256"

echo "== config and Claude Code policy"
[ -f /etc/claudew/config.json ] || install -m 0644 -o root -g root "$HERE/config.example.json" /etc/claudew/config.json
/opt/claudew/bin/claudew -config /etc/claudew/config.json managed-settings > "$BACKUP/managed-settings.json"
install -m 0644 -o root -g root "$BACKUP/managed-settings.json" /etc/claude-code/managed-settings.json

echo "== units (timer installed, not enabled)"
for u in claudew.service claudew.timer claudew-push.socket claudew-push.service; do
  install -m 0644 -o root -g root "$HERE/systemd/$u" "/etc/systemd/system/$u"
done
systemctl daemon-reload
systemctl enable --now claudew-push.socket

echo "== done. Next: credentials with deploy/put-credential.sh, then the checks."
/usr/bin/claude --version
/usr/local/go/bin/go version
