#!/bin/sh
# Install the onthego CLI from GitHub Releases without Node.js.
#
#   curl -fsSL https://raw.githubusercontent.com/peerproblem/onthego/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/peerproblem/onthego/main/install.sh | sh -s -- --version 0.4.0
#
# Environment: ONTHEGO_VERSION (default: latest), ONTHEGO_INSTALL_DIR (default: ~/.local/bin).
set -eu

repo="peerproblem/onthego"
version="${ONTHEGO_VERSION:-latest}"
install_dir="${ONTHEGO_INSTALL_DIR:-$HOME/.local/bin}"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) version="${2:?--version needs a value}"; shift 2 ;;
    --version=*) version="${1#*=}"; shift ;;
    --dir) install_dir="${2:?--dir needs a value}"; shift 2 ;;
    --dir=*) install_dir="${1#*=}"; shift ;;
    -h|--help) sed -n '2,7p' "$0" 2>/dev/null || true; exit 0 ;;
    *) echo "onthego install: unknown option $1" >&2; exit 2 ;;
  esac
done

fail() { echo "onthego install: $*" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported operating system $(uname -s); onthego supports macOS and Linux" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported CPU architecture $(uname -m); onthego supports amd64 and arm64" ;;
esac
asset="onthego-$os-$arch"

version="${version#v}"
if [ "$version" = latest ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  base="https://github.com/$repo/releases/download/v$version"
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  fail "curl or wget is required"
fi
if command -v sha256sum >/dev/null 2>&1; then
  digest() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  digest() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "sha256sum or shasum is required to verify the download"
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/onthego-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

echo "Downloading $asset ($version) from github.com/$repo"
fetch "$base/$asset" "$tmp/onthego" || fail "download failed: $base/$asset"
fetch "$base/$asset.sha256" "$tmp/onthego.sha256" || fail "checksum download failed"
expected=$(cut -d' ' -f1 "$tmp/onthego.sha256")
actual=$(digest "$tmp/onthego")
[ -n "$expected" ] && [ "$expected" = "$actual" ] || fail "checksum mismatch for $asset"

chmod 755 "$tmp/onthego"
"$tmp/onthego" version >/dev/null || fail "the downloaded binary does not run on this machine"
mkdir -p "$install_dir"
mv -f "$tmp/onthego" "$install_dir/onthego"
echo "Installed $("$install_dir/onthego" version) to $install_dir/onthego"

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *)
    echo
    echo "Add $install_dir to your PATH, for example:"
    echo "  echo 'export PATH=\"$install_dir:\$PATH\"' >> ~/.profile"
    ;;
esac
