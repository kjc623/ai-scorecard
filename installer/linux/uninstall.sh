#!/bin/sh
# uninstall.sh - remove Shadow AI Capture. Run it as root:
#
#   sudo sh uninstall.sh [--purge]
#
# Without --purge the state directory and the configuration are kept, so a reinstall resumes as the
# same enrolled device. --purge also removes them: the device credential, the spool and the tenant
# file. Stopping the service removes its interception root from the system trust store.
set -eu

PURGE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --purge) PURGE=1; shift ;;
    -h|--help) sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "uninstall.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" = 0 ] || { echo "uninstall.sh: run as root" >&2; exit 1; }

HOST=com.shadowaicapture.capture_core.json
if command -v systemctl >/dev/null 2>&1; then
  systemctl disable --now shadow-ai-capture.service 2>/dev/null || true
  rm -f /etc/systemd/system/shadow-ai-capture.service
  systemctl daemon-reload
fi
rm -f "/etc/opt/chrome/native-messaging-hosts/$HOST" "/etc/opt/edge/native-messaging-hosts/$HOST"
rm -rf /opt/shadow-ai-capture

if [ "$PURGE" -eq 1 ]; then
  rm -rf /etc/shadow-ai-capture /var/lib/shadow-ai-capture
  echo "uninstalled; state and configuration removed"
else
  echo "uninstalled; kept /etc/shadow-ai-capture and /var/lib/shadow-ai-capture (--purge removes them)"
fi
