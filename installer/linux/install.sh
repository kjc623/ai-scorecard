#!/bin/sh
# install.sh - install Shadow AI Capture from this package. Run it as root from the unpacked folder:
#
#   sudo sh install.sh [--tenant-env FILE]
#
# ShadowAICapture.tenant.env beside this script (or --tenant-env FILE) becomes
# /etc/shadow-ai-capture/tenant.env, replacing an installed one; with neither, an installed one is
# kept, and a device with no tenant file at all gets nothing installed. The binaries, the classifier
# release, the vendor file and the native messaging host are replaced on every install; the state
# directory (the device credential and the spool) is kept.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
TENANT_SRC=""
while [ $# -gt 0 ]; do
  case "$1" in
    --tenant-env) TENANT_SRC="$2"; shift 2 ;;
    -h|--help) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "install.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done

PREFIX=/opt/shadow-ai-capture
CONFIGDIR=/etc/shadow-ai-capture
STATEDIR=/var/lib/shadow-ai-capture
UNIT=/etc/systemd/system/shadow-ai-capture.service
HOST=com.shadowaicapture.capture_core.json

[ "$(id -u)" = 0 ] || { echo "install.sh: run as root" >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "install.sh: systemd is required" >&2; exit 1; }
for f in bin/capture-core bin/classifier-host classifier/manifest.json etc/capture-core.env etc/shadow-ai-capture.service "etc/$HOST"; do
  [ -e "$HERE/$f" ] || { echo "install.sh: $HERE/$f is missing; this is not a complete package" >&2; exit 1; }
done

if [ -z "$TENANT_SRC" ] && [ -f "$HERE/ShadowAICapture.tenant.env" ]; then
  TENANT_SRC="$HERE/ShadowAICapture.tenant.env"
fi
if [ -n "$TENANT_SRC" ] && [ ! -f "$TENANT_SRC" ]; then
  echo "install.sh: $TENANT_SRC does not exist" >&2
  exit 1
fi
if [ -z "$TENANT_SRC" ] && [ ! -f "$CONFIGDIR/tenant.env" ]; then
  echo "install.sh: ShadowAICapture.tenant.env is not beside this script and this device has no tenant" >&2
  echo "  configuration. Run it from the deployment package folder, or pass --tenant-env FILE." >&2
  exit 1
fi

systemctl stop shadow-ai-capture.service 2>/dev/null || true

install -d -m 0755 "$PREFIX/bin" "$CONFIGDIR"
install -d -m 0700 "$STATEDIR"
install -m 0755 "$HERE/bin/capture-core" "$PREFIX/bin/capture-core"
install -m 0755 "$HERE/bin/classifier-host" "$PREFIX/bin/classifier-host"
rm -rf "$PREFIX/classifier"
install -d -m 0755 "$PREFIX/classifier"
cp -R "$HERE/classifier/." "$PREFIX/classifier/"
install -m 0644 "$HERE/etc/capture-core.env" "$CONFIGDIR/capture-core.env"
if [ -n "$TENANT_SRC" ]; then
  install -m 0600 "$TENANT_SRC" "$CONFIGDIR/tenant.env"
fi
for dir in /etc/opt/chrome/native-messaging-hosts /etc/opt/edge/native-messaging-hosts; do
  install -d -m 0755 "$dir"
  install -m 0644 "$HERE/etc/$HOST" "$dir/$HOST"
done
install -m 0644 "$HERE/etc/shadow-ai-capture.service" "$UNIT"

systemctl daemon-reload
systemctl enable shadow-ai-capture.service >/dev/null 2>&1
systemctl restart shadow-ai-capture.service
echo "installed; the service is running (journalctl -u shadow-ai-capture for its log)"
