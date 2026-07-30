#!/bin/sh
# IOLD installer: downloads the pinned release binary, verifies its
# SHA-256 checksum, and installs it idempotently. It never installs
# CUDA, Docker, or vLLM (docs/ARCHITECTURE.md §3 bootstrap path) — those come
# from the supported RunPod template; run `iold doctor` to validate.
#
# Usage:
#   IOLD_VERSION=v0.1.0 sh install.sh
#   sh install.sh            # installs the latest release
set -eu

REPO="demirtechcom/iold"
INSTALL_DIR="${IOLD_INSTALL_DIR:-/usr/local/bin}"
VERSION="${IOLD_VERSION:-}"

fail() { echo "install.sh: $*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
[ "$os" = "linux" ] || fail "unsupported OS '$os'; IOLD v0 supports Linux only"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  *) fail "unsupported architecture '$arch'; IOLD v0 supports amd64 only" ;;
esac

command -v curl >/dev/null || fail "curl is required"
command -v sha256sum >/dev/null || fail "sha256sum is required"

if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
  [ -n "$VERSION" ] || fail "could not resolve the latest release tag"
fi

binary="iold_${VERSION}_linux_${arch}"
base_url="https://github.com/${REPO}/releases/download/${VERSION}"
target="${INSTALL_DIR}/iold"

# Idempotence: skip download when the requested version is installed.
if [ -x "$target" ] && [ "$("$target" version 2>/dev/null)" = "iold ${VERSION}" ]; then
  echo "iold ${VERSION} is already installed at ${target}"
  exit 0
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

echo "Downloading iold ${VERSION} (linux/${arch})..."
curl -fsSL -o "${tmpdir}/${binary}" "${base_url}/${binary}"
curl -fsSL -o "${tmpdir}/checksums.txt" "${base_url}/checksums.txt"

(
  cd "$tmpdir"
  grep " ${binary}\$" checksums.txt >expected.txt || fail "checksum for ${binary} not found in checksums.txt"
  sha256sum -c expected.txt >/dev/null || fail "checksum verification FAILED; aborting install"
)
echo "Checksum verified."

chmod 0755 "${tmpdir}/${binary}"
if [ -w "$INSTALL_DIR" ]; then
  mv "${tmpdir}/${binary}" "$target"
else
  echo "Elevated permissions required for ${INSTALL_DIR}"
  sudo mv "${tmpdir}/${binary}" "$target"
fi

echo "Installed $("$target" version) to ${target}"
echo "Next step: run 'iold doctor' to validate this machine."
