#!/bin/sh
set -eu

out_dir=${1:-dist}
mkdir -p "$out_dir"
out_dir=$(cd "$out_dir" && pwd)
stage_dir=$(mktemp -d "${TMPDIR:-/tmp}/sprout-release.XXXXXX")
trap 'rm -rf "$stage_dir"' EXIT HUP INT TERM

cp LICENSE "$stage_dir/LICENSE"

for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}
  arch=${target#*/}
  rm -f "$stage_dir/sprout" "$stage_dir/sprout.exe"

  if [ "$os" = windows ]; then
    binary=sprout.exe
  else
    binary=sprout
  fi

  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
    -trimpath -ldflags='-s -w' -o "$stage_dir/$binary" ./cmd/sprout

  if [ "$os" = windows ]; then
    (cd "$stage_dir" && zip -q "$out_dir/sprout_${os}_${arch}.zip" "$binary" LICENSE)
  else
    tar -czf "$out_dir/sprout_${os}_${arch}.tar.gz" -C "$stage_dir" "$binary" LICENSE
  fi
done

(
  cd "$out_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum sprout_*.tar.gz sprout_*.zip > checksums.txt
  else
    shasum -a 256 sprout_*.tar.gz sprout_*.zip > checksums.txt
  fi
)
