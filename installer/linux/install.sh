#!/bin/sh
# installer/linux/install.sh - install the Shadow AI Capture endpoint payload from a stage.
#
#   sudo installer/linux/install.sh --stage installer/.stage/linux-amd64
#   installer/linux/install.sh --stage DIR --prefix "$HOME/.sac" --no-service   # rootless dev
#
# The tenant file: a tenant package puts ShadowAICapture.tenant.env beside this script (or pass
# --tenant-env FILE); it is installed as tenant.env, which the wrapper reads after capture-core.env
# so it wins. A supplied file replaces an installed one; with none, an installed one is kept; a
# system install with neither (and no complete --config profile) stops before installing anything,
# as the Windows MSI does.
#
# Two modes, one script:
#   * system  (default, root): the layout docs/05-platform-delivery.md §6.1 describes - binaries
#     under /opt, configuration under /etc, state under /var/lib - plus the systemd unit.
#   * prefix  (--prefix DIR): a rootless layout for a developer machine, with no service manager.
#     The wrapper still reads the config file; the caller points SAC_BINDIR/SAC_CONFIG_FILE at it.
#
# The script never invents a tenant, a token or a key: it installs the template, copies only the
# tenant file it is given, and refuses to overwrite an existing capture-core.env, because an installer
# that silently resets a device's identity is worse than one that stops.
set -eu

STAGE=""
PREFIX=""
CONFIG_SRC=""
TENANT_SRC=""
NO_SERVICE=0
NO_START=0

while [ $# -gt 0 ]; do
  case "$1" in
    --stage) STAGE="$2"; shift 2 ;;
    --prefix) PREFIX="$2"; shift 2 ;;
    --config) CONFIG_SRC="$2"; shift 2 ;;
    --tenant-env) TENANT_SRC="$2"; shift 2 ;;
    --no-service) NO_SERVICE=1; shift ;;
    --no-start) NO_START=1; shift ;;
    -h|--help) sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "install.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done

[ -n "$STAGE" ] || { echo "install.sh: --stage DIR is required" >&2; exit 2; }
STAGE=$(cd "$STAGE" && pwd)
for f in bin/capture-core bin/classifier-host bin/capture-core-run etc/capture-core.env.example; do
  [ -e "$STAGE/$f" ] || { echo "install.sh: $STAGE/$f is missing; run node installer/build.mjs first" >&2; exit 1; }
done

if [ -n "$PREFIX" ]; then
  MODE=prefix
  PREFIX=$(mkdir -p "$PREFIX" && cd "$PREFIX" && pwd)
  BINDIR="$PREFIX/bin"
  CONFIGDIR="$PREFIX/etc"
  STATEDIR="$PREFIX/state"
  LOGDIR="$PREFIX/log"
  DATADIR="$PREFIX/share"
  SERVICE=0
else
  MODE=system
  BINDIR=/opt/shadow-ai-capture/bin
  CONFIGDIR=/etc/shadow-ai-capture
  STATEDIR=/var/lib/shadow-ai-capture
  LOGDIR=/var/log/shadow-ai-capture
  DATADIR=/usr/share/shadow-ai-capture
  SERVICE=1
fi
[ "$NO_SERVICE" -eq 1 ] && SERVICE=0

if [ "$MODE" = system ] && [ "$(id -u)" != 0 ]; then
  echo "install.sh: a system install needs root; re-run with sudo, or use --prefix DIR for a dev install" >&2
  exit 1
fi

HERE=$(cd "$(dirname "$0")" && pwd)
if [ -z "$TENANT_SRC" ] && [ -f "$HERE/ShadowAICapture.tenant.env" ]; then
  TENANT_SRC="$HERE/ShadowAICapture.tenant.env"
fi
TENANT_DEST="$CONFIGDIR/tenant.env"
if [ -n "$TENANT_SRC" ] && [ ! -f "$TENANT_SRC" ]; then
  echo "install.sh: --tenant-env $TENANT_SRC does not exist" >&2
  exit 1
fi
if [ "$MODE" = system ] && [ -z "$TENANT_SRC" ] && [ -z "$CONFIG_SRC" ] && [ ! -f "$TENANT_DEST" ]; then
  echo "install.sh: ShadowAICapture.tenant.env was not found beside this script and this device has no" >&2
  echo "  tenant configuration installed ($TENANT_DEST). Run it from the folder of the deployment package," >&2
  echo "  or pass --tenant-env FILE. Nothing was installed." >&2
  exit 1
fi

echo "install ($MODE):"
mkdir -p "$BINDIR" "$CONFIGDIR" "$STATEDIR" "$LOGDIR" "$DATADIR"

install_file() { # src dest mode
  if command -v install >/dev/null 2>&1; then
    install -m "$3" "$1" "$2"
  else
    cp "$1" "$2"; chmod "$3" "$2"
  fi
}

install_file "$STAGE/bin/capture-core" "$BINDIR/capture-core" 0755
install_file "$STAGE/bin/classifier-host" "$BINDIR/classifier-host" 0755
install_file "$STAGE/bin/capture-core-run" "$BINDIR/capture-core-run" 0755
[ -f "$STAGE/install-manifest.json" ] && install_file "$STAGE/install-manifest.json" "$DATADIR/install-manifest.json" 0644
[ -f "$STAGE/README.txt" ] && install_file "$STAGE/README.txt" "$DATADIR/README.txt" 0644
echo "  binaries   $BINDIR"
echo "  state      $STATEDIR"

# The configuration is the per-tenant enrolment profile. Install the template once; never overwrite.
CONFIG_DEST="$CONFIGDIR/capture-core.env"
if [ -f "$CONFIG_DEST" ]; then
  echo "  config     $CONFIG_DEST (kept: already exists)"
elif [ -n "$CONFIG_SRC" ]; then
  [ -f "$CONFIG_SRC" ] || { echo "install.sh: --config $CONFIG_SRC does not exist" >&2; exit 1; }
  install_file "$CONFIG_SRC" "$CONFIG_DEST" 0600
  echo "  config     $CONFIG_DEST (from $CONFIG_SRC)"
else
  install_file "$STAGE/etc/capture-core.env.example" "$CONFIG_DEST" 0600
  echo "  config     $CONFIG_DEST (template; fill it in or deliver one with --config)"
fi

if [ -n "$TENANT_SRC" ]; then
  install_file "$TENANT_SRC" "$TENANT_DEST" 0600
  echo "  tenant     $TENANT_DEST (from $TENANT_SRC)"
elif [ -f "$TENANT_DEST" ]; then
  echo "  tenant     $TENANT_DEST (kept: already exists)"
fi

if [ "$SERVICE" -eq 1 ]; then
  UNIT=/etc/systemd/system/shadow-ai-capture.service
  install_file "$STAGE/etc/service-definition" "$UNIT" 0644
  echo "  unit       $UNIT"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl enable shadow-ai-capture.service >/dev/null 2>&1 || true
    if [ "$NO_START" -eq 0 ]; then
      systemctl restart shadow-ai-capture.service && echo "  service    started" || echo "  service    FAILED to start; see: journalctl -u shadow-ai-capture -e" >&2
    fi
  else
    echo "  service    systemctl not found; unit installed but not enabled"
  fi
fi

echo
echo "installed. run the wrapper with SAC_BINDIR/SAC_CONFIG_FILE pointing at the layout, or manage"
echo "shadow-ai-capture.service. uninstall with: installer/linux/uninstall.sh $( [ -n "$PREFIX" ] && echo "--prefix $PREFIX" )"
