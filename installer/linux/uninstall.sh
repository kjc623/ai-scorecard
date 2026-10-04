#!/bin/sh
# installer/linux/uninstall.sh - reverse of install.sh.
#
#   sudo installer/linux/uninstall.sh --purge
#   installer/linux/uninstall.sh --prefix "$HOME/.sac" --purge
#
# The default keeps the state directory (the spool and any sealed credential) and the configuration,
# so a reinstall resumes the same device rather than silently minting a new identity. --purge removes
# them, which is the "clean uninstall" of docs/05-platform-delivery.md §6.4 and is deliberately a
# choice, not a default.

set -eu
PREFIX=""
PURGE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) PREFIX="$2"; shift 2 ;;
    --purge) PURGE=1; shift ;;
    -h|--help) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "uninstall.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done

if [ -n "$PREFIX" ]; then
  PREFIX=$(cd "$PREFIX" 2>/dev/null && pwd || echo "$PREFIX")
  BINDIR="$PREFIX/bin"; CONFIGDIR="$PREFIX/etc"; STATEDIR="$PREFIX/state"; DATADIR="$PREFIX/share"; LOGDIR="$PREFIX/log"
  SERVICE=0
else
  BINDIR=/opt/shadow-ai-capture; CONFIGDIR=/etc/shadow-ai-capture; STATEDIR=/var/lib/shadow-ai-capture; DATADIR=/usr/share/shadow-ai-capture; LOGDIR=/var/log/shadow-ai-capture
  SERVICE=1
fi

if [ "$SERVICE" -eq 1 ]; then
  if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now shadow-ai-capture.service 2>/dev/null || true
    systemctl daemon-reload || true
  fi
  rm -f /etc/systemd/system/shadow-ai-capture.service
  # Binaries only; the state and config are handled below by the same choice.
  rm -rf "$BINDIR"
  rmdir "$(dirname "$BINDIR")" 2>/dev/null || true
else
  rm -rf "$BINDIR"
fi
rm -rf "$DATADIR"

if [ "$PURGE" -eq 1 ]; then
  rm -rf "$STATEDIR" "$CONFIGDIR" "$LOGDIR"
  echo "uninstalled (purged: state, config and logs removed)"
else
  echo "uninstalled (kept: $STATEDIR and $CONFIGDIR; re-run with --purge to remove them)"
fi
