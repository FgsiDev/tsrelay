#!/usr/bin/env bash
# Build TSRelay untuk semua VPS + Termux (static, tanpa CGO)
set -euo pipefail

cd "$(dirname "$0")"

[ -f go.mod ] || go mod init tsrelay
go mod tidy

export CGO_ENABLED=0
export GOOS=linux
mkdir -p dist

build() {
  local arch="$1" arm="$2" name="$3"
  echo ">> $name"
  GOARCH="$arch" GOARM="$arm" go build -trimpath -ldflags "-s -w" -o "dist/tsrelay-$name" .
}

build amd64 ""  linux-amd64     # VPS x86_64
build 386   ""  linux-386       # VPS x86 32-bit
build arm64 ""  linux-arm64     # Termux 64-bit / VPS ARM
build arm   7   linux-armv7     # Termux 32-bit (hampir semua HP)
build arm   6   linux-armv6     # ARM 32-bit lama / Raspberry Pi lama

ls -lh dist
