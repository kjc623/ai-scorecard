#!/bin/sh
# build-pkg.sh - build ShadowAICapture.pkg from a darwin stage. Run it on macOS:
#
#   node installer/build.mjs --os darwin --arch arm64 --version 1.4.0 ...
#   installer/macos/build-pkg.sh --stage installer/.stage/darwin-arm64 --version 1.4.0 --out installer/dist \
#     [--sign "Developer ID Installer: <organisation> (<team id>)"]
#
# The package installs the binaries and the classifier release under /usr/local/opt/shadow-ai-capture,
# the vendor file under /usr/local/etc/shadow-ai-capture, the LaunchDaemon, and the native messaging
# host for Chrome and Edge. The tenant file travels beside the .pkg: preinstall refuses an install
# with neither it nor an installed tenant.env, and postinstall copies it in.
set -eu

STAGE=""; OUT=""; VERSION=""; SIGN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --stage) STAGE="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --sign) SIGN="$2"; shift 2 ;;
    -h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "build-pkg.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$STAGE" ] && [ -n "$OUT" ] && [ -n "$VERSION" ] || { echo "build-pkg.sh: --stage, --out and --version are required" >&2; exit 2; }
command -v pkgbuild >/dev/null 2>&1 || { echo "build-pkg.sh: pkgbuild not found; build the package on macOS" >&2; exit 1; }

HOST=com.shadowaicapture.capture_core.json
PLIST=com.shadowaicapture.capture-core.plist
for f in bin/capture-core bin/classifier-host classifier/manifest.json etc/capture-core.env "etc/$PLIST" "etc/$HOST"; do
  [ -e "$STAGE/$f" ] || { echo "build-pkg.sh: $STAGE/$f is missing; build the stage with node installer/build.mjs --os darwin" >&2; exit 1; }
done

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
R="$WORK/root"
OPT="$R/usr/local/opt/shadow-ai-capture"
install -d "$OPT/bin" "$OPT/classifier" "$R/usr/local/etc/shadow-ai-capture" "$R/Library/LaunchDaemons" \
  "$R/Library/Google/Chrome/NativeMessagingHosts" "$R/Library/Microsoft Edge/NativeMessagingHosts"
install -m 0755 "$STAGE/bin/capture-core" "$OPT/bin/capture-core"
install -m 0755 "$STAGE/bin/classifier-host" "$OPT/bin/classifier-host"
cp -R "$STAGE/classifier/." "$OPT/classifier/"
install -m 0644 "$STAGE/etc/capture-core.env" "$R/usr/local/etc/shadow-ai-capture/capture-core.env"
install -m 0644 "$STAGE/etc/$PLIST" "$R/Library/LaunchDaemons/$PLIST"
install -m 0644 "$STAGE/etc/$HOST" "$R/Library/Google/Chrome/NativeMessagingHosts/$HOST"
install -m 0644 "$STAGE/etc/$HOST" "$R/Library/Microsoft Edge/NativeMessagingHosts/$HOST"

# pkgbuild runs the scripts only when they are executable, whatever the checkout preserved.
install -d "$WORK/scripts"
install -m 0755 "$HERE/scripts/preinstall" "$HERE/scripts/postinstall" "$WORK/scripts/"

mkdir -p "$OUT"
COMPONENT="$WORK/ShadowAICapture-component.pkg"
pkgbuild --root "$R" --identifier com.shadowaicapture.capture-core --version "$VERSION" \
  --ownership recommended --scripts "$WORK/scripts" "$COMPONENT"
if [ -n "$SIGN" ]; then
  productbuild --package "$COMPONENT" --sign "$SIGN" "$OUT/ShadowAICapture.pkg"
else
  productbuild --package "$COMPONENT" "$OUT/ShadowAICapture.pkg"
fi
echo "built $OUT/ShadowAICapture.pkg"
