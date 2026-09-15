#!/usr/bin/env sh
# install-rig.sh — one-shot userland installer for the rig binary.
#
# Usage (the README one-liner pins this script at the git sha of the latest
# release; version selection still floats to the latest release by default):
#
#   curl -fsSL https://raw.githubusercontent.com/brutasse/rig/<sha>/scripts/install-rig.sh | sh
#   curl -fsSL ... | sh -s -- v0.2.0        # install a specific version
#
# The script fetches the rig binary for the current platform from the GitHub
# release, verifies it against the release's SHA256SUMS, and installs it to
# $INSTALL_DIR (default: ~/.local/bin). The resolver kernel jar is not
# installed here — the rig binary fetches it on first use, hash-verified.
#
# Env:
#   INSTALL_DIR  installation directory (default: $HOME/.local/bin)
set -eu

REPO="brutasse/rig"
VERSION="${1:-}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

command -v curl >/dev/null 2>&1 || { echo "install-rig: curl is required" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux | darwin) ;;
  *) echo "install-rig: unsupported OS '$os' (see https://github.com/$REPO/releases)" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "install-rig: unsupported architecture '$arch'" >&2; exit 1 ;;
esac

BIN="rig-$os-$arch"

if [ -z "$VERSION" ]; then
  latest=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest")
  VERSION=$(printf '%s' "$latest" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p')
  [ -n "$VERSION" ] || { echo "install-rig: could not determine the latest release" >&2; exit 1; }
fi
case "$VERSION" in
  v[0-9]*) ;;
  *) echo "install-rig: bad version '$VERSION' (want vX.Y.Z)" >&2; exit 1 ;;
esac

BASE="https://github.com/$REPO/releases/download/$VERSION"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "install-rig: rig $VERSION ($BIN)"
curl -fsSL "$BASE/$BIN" -o "$TMP/$BIN"
curl -fsSL "$BASE/SHA256SUMS" -o "$TMP/SHA256SUMS"

WANT=$(awk -v f="$BIN" '$2 == f {print $1}' "$TMP/SHA256SUMS")
[ -n "$WANT" ] || { echo "install-rig: $BIN not listed in SHA256SUMS" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum "$TMP/$BIN" | cut -d' ' -f1)
else
  GOT=$(shasum -a 256 "$TMP/$BIN" | cut -d' ' -f1)
fi
[ "$GOT" = "$WANT" ] || { echo "install-rig: sha256 mismatch for $BIN (got $GOT)" >&2; exit 1; }

mkdir -p "$INSTALL_DIR"
DEST_TMP="$INSTALL_DIR/.rig-$$.tmp"
cp "$TMP/$BIN" "$DEST_TMP"
chmod 0755 "$DEST_TMP"
mv -f "$DEST_TMP" "$INSTALL_DIR/rig"

echo "installed $INSTALL_DIR/rig ($VERSION)"
if command -v rig >/dev/null 2>&1 && [ "$(command -v rig)" != "$INSTALL_DIR/rig" ]; then
  echo "note: '$(command -v rig)' is first on PATH; add $INSTALL_DIR to PATH or remove the old binary"
fi
echo "the resolver kernel jar is fetched by rig on first use (hash-verified)"
