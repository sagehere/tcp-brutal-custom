#!/usr/bin/env bash
set -Eeuo pipefail

key="${1:-}"
dir="${2:-build}"
expected='249a5abded1a497f8fe67f4cf5cd8e47d127b9cee2d9b1eac23042b7edfeceff'

[[ -n "$key" && -f "$key" ]] || { echo "Usage: bash scripts/sign-release.sh PRIVATE_KEY [BUILD_DIR]" >&2; exit 2; }
[[ -f "$dir/hashes.txt" ]] || { echo "Missing $dir/hashes.txt" >&2; exit 1; }

for file in tcp-brutal-custom-linux-amd64 tcp-brutal-custom-linux-arm64 tcp-brutal-custom.dkms.tar.gz install.sh; do
  [[ -f "$dir/$file" ]] || { echo "Missing $dir/$file" >&2; exit 1; }
done

actual=$(openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | openssl dgst -sha256 | awk '{print $2}')
[[ "$actual" == "$expected" ]] || { echo "Signing key fingerprint mismatch" >&2; exit 1; }

(
  cd "$dir"
  sha256sum -c hashes.txt
)

openssl pkeyutl -sign -rawin -inkey "$key" -in "$dir/hashes.txt" -out "$dir/hashes.txt.sig"
openssl pkeyutl -verify -rawin -pubin -inkey keys/release-signing-pub.pem -sigfile "$dir/hashes.txt.sig" -in "$dir/hashes.txt" >/dev/null

echo "Signed: $dir/hashes.txt.sig"
