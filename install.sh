#!/usr/bin/env sh
# kubectl-upgrade installer.
#
# Usage:
#   curl -sSL https://raw.githubusercontent.com/saiyam1814/upgrade/main/install.sh | sh
#
# Honors:
#   INSTALL_DIR=/usr/local/bin   (default: /usr/local/bin or ~/.local/bin)
#   VERSION=v0.1.0               (default: latest)
set -eu

REPO="saiyam1814/upgrade"
BIN="kubectl-upgrade"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

if [ -z "${VERSION:-}" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -o '"tag_name": "[^"]*"' | head -1 | cut -d'"' -f4)
fi
if [ -z "$VERSION" ]; then
  echo "could not resolve latest version; set VERSION=vX.Y.Z" >&2
  exit 1
fi

if [ -z "${INSTALL_DIR:-}" ]; then
  if [ -w /usr/local/bin ] 2>/dev/null; then
    INSTALL_DIR=/usr/local/bin
  else
    INSTALL_DIR="$HOME/.local/bin"
    mkdir -p "$INSTALL_DIR"
  fi
fi

URL="https://github.com/$REPO/releases/download/$VERSION/${BIN}_${OS}_${ARCH}.tar.gz"

echo "Downloading $URL"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

curl -fsSL "$URL" | tar -xz -C "$TMP"
install -m 0755 "$TMP/$BIN" "$INSTALL_DIR/$BIN"

echo "✓ Installed $BIN $VERSION to $INSTALL_DIR/$BIN"
echo
echo "Try:"
echo "  $BIN --help"
echo "  kubectl upgrade --help     # if INSTALL_DIR is on PATH"
