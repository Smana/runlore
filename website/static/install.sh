#!/bin/sh
# RunLore installer — downloads the released `lore` binary for this OS/arch,
# verifies its SHA-256 against the published checksums, and installs it.
#
#   curl -fsSL https://runlore.io/install.sh | sh
#
# Environment:
#   LORE_VERSION      pin a release tag, e.g. v0.11.0 (default: latest)
#   LORE_INSTALL_DIR  install target (default: /usr/local/bin, else ~/.local/bin)
#
# This script is auditable at
# https://github.com/Smana/runlore/blob/main/website/static/install.sh
# It never runs sudo, never sends telemetry, and only ever writes to the
# chosen install dir and a temp dir it cleans up on exit.
set -eu

REPO="Smana/runlore"
VERSION="${LORE_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux|darwin) ;;
  *) echo "unsupported OS: $os — build from source with 'go install github.com/Smana/runlore/cmd/lore@latest'" >&2; exit 1 ;;
esac

# macOS has no sha256sum; shasum -a 256 is the equivalent everywhere Apple ships.
if ! command -v sha256sum >/dev/null 2>&1; then
  sha256sum() { shasum -a 256 "$@"; }
fi

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$VERSION" ] || { echo "could not resolve the latest release tag" >&2; exit 1; }
fi

# Matches .goreleaser.yaml: project_name=runlore, name_template
# {{.ProjectName}}_{{.Version}}_{{.Os}}_{{.Arch}}, tar.gz. Note the archive is named
# for the PROJECT (runlore) while the binary inside is `lore`.
archive="runlore_${VERSION#v}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading $archive ($VERSION)"
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

# Verify BEFORE extracting. A checksum mismatch means a corrupted or tampered
# download, and extracting it anyway would defeat the point of checking — abort
# with nothing installed rather than silently trusting the bytes.
( cd "$tmp" && grep " $archive\$" checksums.txt | sha256sum -c - >/dev/null ) \
  || { echo "checksum verification FAILED for $archive — aborting, nothing installed" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp" lore

# Pick an install dir that needs no elevated privileges. This script never
# invokes sudo: if the preferred/requested dir can't be written to, it falls
# back to a per-user directory instead of asking for root.
requested="${LORE_INSTALL_DIR:-/usr/local/bin}"
dir="$requested"
mkdir -p "$dir" 2>/dev/null || true
if [ ! -w "$dir" ]; then
  dir="$HOME/.local/bin"
  mkdir -p "$dir"
  echo "note: $requested is not writable — installing to $dir instead" >&2
fi
install -m 0755 "$tmp/lore" "$dir/lore"

echo "installed $("$dir/lore" version) to $dir/lore"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: $dir is not on your PATH — add it, e.g. export PATH=\"$dir:\$PATH\"" ;;
esac
echo
echo "Try it with no cluster and no API key:"
echo "  lore demo investigate --offline default"
echo
echo "Verify the release signature (optional):"
echo "  cosign verify-blob --bundle checksums.txt.bundle \\"
echo "    --certificate-identity-regexp 'https://github.com/$REPO/.github/workflows/release-binaries.yml@.*' \\"
echo "    --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt"
