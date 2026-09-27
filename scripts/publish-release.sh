#!/usr/bin/env bash
set -Eeuo pipefail

tag="${1:-}"
key="${2:-}"
dir="${3:-build}"

[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "Usage: bash scripts/publish-release.sh vX.Y.Z PRIVATE_KEY [BUILD_DIR]" >&2
  exit 2
}
command -v gh >/dev/null || { echo "gh CLI required" >&2; exit 1; }
command -v openssl >/dev/null || { echo "openssl required" >&2; exit 1; }

bash scripts/sign-release.sh "$key" "$dir"

gh release create "$tag" \
  "$dir/tcp-brutal-custom-linux-amd64" \
  "$dir/tcp-brutal-custom-linux-arm64" \
  "$dir/tcp-brutal-custom.dkms.tar.gz" \
  "$dir/install.sh" \
  "$dir/hashes.txt" \
  "$dir/hashes.txt.sig" \
  --verify-tag \
  --title "$tag" \
  --generate-notes

echo "Published signed release $tag"
