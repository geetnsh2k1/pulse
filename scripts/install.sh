#!/bin/sh
# pulse installer — downloads the right binary from the latest GitHub
# release. Usage:  curl -fsSL https://raw.githubusercontent.com/geetnsh2k1/pulse/master/scripts/install.sh | sh
set -eu

REPO="geetnsh2k1/pulse"
INSTALL_DIR="${PULSE_INSTALL_DIR:-/usr/local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64) arch="amd64" ;;
  aarch64 | arm64) arch="arm64" ;;
  *) echo "✗ unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin | linux) ;;
  *) echo "✗ unsupported OS: $os (Windows: grab the .zip from the releases page)" >&2; exit 1 ;;
esac

echo "⚡ pulse installer — $os/$arch"

tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
  grep '"tag_name"' | head -1 | cut -d '"' -f 4)
[ -n "$tag" ] || { echo "✗ couldn't resolve the latest release" >&2; exit 1; }
version=${tag#v}

url="https://github.com/$REPO/releases/download/$tag/pulse_${version}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "  downloading $tag…"
curl -fsSL "$url" -o "$tmp/pulse.tar.gz"
tar -xzf "$tmp/pulse.tar.gz" -C "$tmp"

dest="$INSTALL_DIR/pulse"
if [ -w "$INSTALL_DIR" ]; then
  install -m 0755 "$tmp/pulse" "$dest"
else
  echo "  $INSTALL_DIR needs sudo:"
  sudo install -m 0755 "$tmp/pulse" "$dest"
fi

echo "✓ installed $("$dest" version 2>/dev/null | sed -n 2p | awk '{print $1, $2}' || echo "pulse $version") → $dest"
echo "  new here? run: pulse tour"
