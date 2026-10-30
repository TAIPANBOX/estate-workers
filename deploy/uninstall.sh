#!/bin/bash
# Remove estate workers from the host. Run as root. Keeps the backups in
# /var/backups/claudew-install-* so the state before the install can be
# compared. GitHub App and Console key are revoked by their owner, not here.
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }

systemctl disable --now claudew.timer claudew-push.socket claudew-proxy.socket 2>/dev/null || true
systemctl stop claudew.service claudew-push.service claudew-proxy.service 2>/dev/null || true
systemctl disable --now claudew-egress.service 2>/dev/null || true
rm -f /etc/systemd/system/claudew.service /etc/systemd/system/claudew.timer \
      /etc/systemd/system/claudew-push.socket /etc/systemd/system/claudew-push.service \
      /etc/systemd/system/claudew-egress.service \
      /etc/systemd/system/claudew-proxy.socket /etc/systemd/system/claudew-proxy.service
systemctl daemon-reload

rm -f /etc/credstore.encrypted/anthropic-workers.cred /etc/credstore.encrypted/github-app-workers.cred
if [ -z "$(ls -A /etc/credstore.encrypted 2>/dev/null)" ]; then
  rm -f /var/lib/systemd/credential.secret
fi

rm -rf /etc/claude-code /etc/claudew /opt/claudew /var/lib/claudew /var/lib/claudew-gh
rm -f /var/lib/prometheus/node-exporter/claudew.prom

getent passwd claudew-proxy >/dev/null && userdel claudew-proxy
getent group claudew-proxy >/dev/null && groupdel claudew-proxy
getent passwd claudew-gh >/dev/null && userdel claudew-gh
getent passwd claudew >/dev/null && userdel claudew
getent group claudew-gh >/dev/null && groupdel claudew-gh
getent group claudew >/dev/null && groupdel claudew

apt-get purge -y -q claude-code bubblewrap socat make gcc
apt-get autoremove -y -q
rm -f /etc/apt/sources.list.d/claude-code.list /etc/apt/keyrings/claude-code.asc
apt-get update -q
rm -rf /usr/local/go

echo "removed. Compare with the newest /var/backups/claudew-install-*/before.txt"
