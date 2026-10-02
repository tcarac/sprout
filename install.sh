#!/bin/sh
set -eu

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo 'Sprout installer supports macOS and Linux. Download Windows binaries from GitHub Releases.' >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo 'Unsupported CPU architecture.' >&2; exit 1 ;;
esac

asset="sprout_${os}_${arch}.tar.gz"
base='https://github.com/tcarac/sprout/releases/latest/download'
temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/sprout-install.XXXXXX")
trap 'rm -rf "$temp_dir"' EXIT HUP INT TERM

curl -fsSL "$base/$asset" -o "$temp_dir/$asset"
curl -fsSL "$base/checksums.txt" -o "$temp_dir/checksums.txt"

expected=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$temp_dir/checksums.txt")
if [ -z "$expected" ]; then
  echo "Checksum missing for $asset." >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$temp_dir/$asset" | awk '{ print $1 }')
else
  actual=$(shasum -a 256 "$temp_dir/$asset" | awk '{ print $1 }')
fi

if [ "$actual" != "$expected" ]; then
  echo "Checksum mismatch for $asset." >&2
  exit 1
fi

tar -xzf "$temp_dir/$asset" -C "$temp_dir" sprout
install_dir=${SPROUT_INSTALL_DIR:-"$HOME/.local/bin"}
mkdir -p "$install_dir"
install -m 755 "$temp_dir/sprout" "$install_dir/sprout"
printf 'Installed Sprout to %s/sprout\n' "$install_dir"
printf 'Make sure %s is on your PATH.\n' "$install_dir"
