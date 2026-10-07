#!/usr/bin/env bash
# TSRelay installer: auto-detect arsitektur lalu download binary dari GitHub Release terbaru
#
# Pakai:
#   curl -fsSL https://raw.githubusercontent.com/FgsiDev/tsrelay/main/install.sh | bash
#   curl -fsSL .../install.sh | bash -s -- nama-output
#
# Env opsional:
#   REPO=user/repo   VERSION=v1.0.5 (default: latest)   OUT=nama-file
set -euo pipefail

REPO="${REPO:-FgsiDev/tsrelay}"
VERSION="${VERSION:-latest}"
OUT="${1:-${OUT:-tsrelay}}"

# ---- deteksi arsitektur ----
case "$(uname -m)" in
  x86_64|amd64)          ARCH="amd64" ;;
  i386|i486|i586|i686)   ARCH="386" ;;
  aarch64|arm64)         ARCH="arm64" ;;
  armv8l|armv7l|armv7*)  ARCH="armv7" ;;   # termux 32-bit
  armv6l|armv6*)         ARCH="armv6" ;;
  *) echo "Arsitektur tidak didukung: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$(uname -s)" != "Linux" ] && [ "$(uname -o 2>/dev/null)" != "Android" ]; then
  echo "OS tidak didukung: $(uname -s)" >&2
  exit 1
fi

NAME="tsrelay-linux-${ARCH}"
if [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/${REPO}/releases/latest/download"
else
  BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi

# ---- downloader ----
fetch() { # url dest
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 --progress-bar -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q --show-progress -O "$2" "$1"
  else
    echo "Butuh curl atau wget (Termux: pkg install curl)" >&2
    exit 1
  fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo ">> Device  : $(uname -m) -> ${ARCH}"
echo ">> Download: ${BASE}/${NAME}"
fetch "${BASE}/${NAME}" "${TMP}/${NAME}"

# ---- verifikasi checksum (kalau tersedia) ----
if command -v sha256sum >/dev/null 2>&1 && fetch "${BASE}/SHA256SUMS.txt" "${TMP}/SHA256SUMS.txt" 2>/dev/null; then
  EXPECT="$(grep " ${NAME}\$" "${TMP}/SHA256SUMS.txt" | awk '{print $1}')"
  ACTUAL="$(sha256sum "${TMP}/${NAME}" | awk '{print $1}')"
  if [ -n "$EXPECT" ] && [ "$EXPECT" != "$ACTUAL" ]; then
    echo "Checksum tidak cocok! Download dibatalkan." >&2
    exit 1
  fi
  echo ">> Checksum OK"
fi

mv -f "${TMP}/${NAME}" "$OUT"
chmod +x "$OUT"

echo ">> Selesai : $(pwd)/${OUT}"
echo
echo "Jalankan:"
echo "  export TS_AUTHKEY=tskey-xxxx"
echo "  ./${OUT} --mode target --listen :3982 --target 100.x.x.x:8080"
