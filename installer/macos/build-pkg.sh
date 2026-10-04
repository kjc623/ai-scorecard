#!/bin/sh
# installer/macos/build-pkg.sh - build ShadowAICapture.pkg from a stage.
#
#   node installer/build.mjs --os darwin --arch arm64
#   installer/macos/build-pkg.sh --stage installer/.stage/darwin-arm64 --out installer/dist
#
# NOT VERIFIED ON THIS HOST: there is no macOS, no pkgbuild and no productbuild here. This script is
# the macOS half of the platform contract, kept behaviourally aligned with the Linux install and the
# WiX source through installer/manifest.mjs; only a macOS host proves it. When pkgbuild is absent it
# prints the commands it would run and exits non-zero rather than pretending to have built a package.
#
# What it installs, matching docs/05-platform-delivery.md §6.1:
#   /usr/local/opt/shadow-ai-capture/bin/{capture-core,classifier-host,capture-core-run}
#   /usr/local/etc/shadow-ai-capture/capture-core.env.example
#   /Library/LaunchDaemons/com.shadowaicapture.capture-core.plist
# The per-tenant profile is delivered by the customer's Jamf configuration profile, not the PKG.
set -eu

STAGE=""; OUT="installer/dist"; CONFIG_SRC=""; SIGN=""; VERSION="0.1.0"
while [ $# -gt 0 ]; do
  case "$1" in
    --stage) STAGE="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --config) CONFIG_SRC="$2"; shift 2 ;;
    --sign) SIGN="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "build-pkg.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$STAGE" ] || { echo "build-pkg.sh: --stage DIR is required" >&2; exit 2; }
[ -e "$STAGE/bin/capture-core" ] || { echo "build-pkg.sh: $STAGE is not a darwin stage; run node installer/build.mjs --os darwin" >&2; exit 1; }

HERE=$(cd "$(dirname "$0")" && pwd)

if ! command -v pkgbuild >/dev/null 2>&1; then
  cat >&2 <<EOF
build-pkg.sh: pkgbuild not found - this host cannot build a macOS PKG. On a macOS host run:

  node installer/build.mjs --os darwin --arch arm64
  installer/macos/build-pkg.sh --stage installer/.stage/darwin-arm64 --out installer/dist

The script stages these files, then runs pkgbuild + productbuild:
  bin/capture-core, bin/classifier-host, bin/capture-core-run
  etc/capture-core.env.example
  /Library/LaunchDaemons/com.shadowaicapture.capture-core.plist
EOF
  exit 1
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
ROOT="$WORK/root"
mkdir -p "$ROOT/usr/local/opt/shadow-ai-capture/bin" \
         "$ROOT/usr/local/etc/shadow-ai-capture" \
         "$ROOT/Library/LaunchDaemons"

install -m 0755 "$STAGE/bin/capture-core" "$ROOT/usr/local/opt/shadow-ai-capture/bin/capture-core"
install -m 0755 "$STAGE/bin/classifier-host" "$ROOT/usr/local/opt/shadow-ai-capture/bin/classifier-host"
install -m 0755 "$STAGE/bin/capture-core-run" "$ROOT/usr/local/opt/shadow-ai-capture/bin/capture-core-run"
install -m 0644 "$STAGE/etc/capture-core.env.example" "$ROOT/usr/local/etc/shadow-ai-capture/capture-core.env.example"
install -m 0644 "$STAGE/etc/service-definition.plist" "$ROOT/Library/LaunchDaemons/com.shadowaicapture.capture-core.plist"
if [ -n "$CONFIG_SRC" ]; then
  install -m 0600 "$CONFIG_SRC" "$ROOT/usr/local/etc/shadow-ai-capture/capture-core.env"
fi

mkdir -p "$OUT"
COMPONENT="$OUT/ShadowAICapture-component.pkg"
pkgbuild --root "$ROOT" --identifier com.shadowaicapture.capture-core --version "$VERSION" \
  --scripts "$HERE/scripts" "$COMPONENT"

PKG="$OUT/ShadowAICapture.pkg"
if [ -n "$SIGN" ]; then
  productbuild --package "$COMPONENT" --sign "$SIGN" "$PKG"
else
  echo "build-pkg.sh: WARNING - unsigned development PKG; pass --sign 'Developer ID Installer: ...' for a release" >&2
  productbuild --package "$COMPONENT" "$PKG"
fi
echo "built $PKG"
