#!/usr/bin/env bash
set -Eeuo pipefail

repo='https://github.com/sagehere/tcp-brutal-custom'
tag="${TCP_BRUTAL_RELEASE_TAG:-latest}"
fingerprint='b1a16baa2d9c68fdff5594e1261e0668f45b65253bf454b7c27025265b99bc1d'

if (( EUID != 0 )); then
  echo 'Run as root (for example: curl ... | sudo bash)' >&2
  exit 1
fi

case "$tag" in
  latest) release="$repo/releases/latest/download" ;;
  v[0-9]*.[0-9]*.[0-9]*) release="$repo/releases/download/$tag" ;;
  *) echo "Invalid TCP_BRUTAL_RELEASE_TAG: $tag" >&2; exit 2 ;;
esac

command -v curl >/dev/null || { echo 'curl required' >&2; exit 1; }
command -v sha256sum >/dev/null || { echo 'sha256sum required' >&2; exit 1; }

if ! command -v openssl >/dev/null; then
  if [[ -r /etc/os-release ]]; then
    . /etc/os-release
  fi
  case "${ID:-}:${VERSION_ID:-}" in
    debian:12|debian:13|ubuntu:22.04|ubuntu:24.04)
      apt-get update
      DEBIAN_FRONTEND=noninteractive apt-get install -y openssl
      ;;
    *)
      echo 'openssl is required' >&2
      exit 1
      ;;
  esac
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

cat >"$tmp/release-signing-pub.pem" <<'EOF'
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEA32QwT1Z9jNobN7jIIlY2KZazDgzidVQOx3/dOLp0AIs=
-----END PUBLIC KEY-----
EOF

actual=$(openssl pkey -pubin -in "$tmp/release-signing-pub.pem" -outform DER 2>/dev/null |
  openssl dgst -sha256 | awk '{print $2}')
[[ "$actual" == "$fingerprint" ]] || {
  echo 'Embedded release key fingerprint mismatch' >&2
  exit 1
}

echo "TCP Brutal Custom release key: $fingerprint"
echo "Downloading signed release metadata..."
curl -fsSL --retry 3 "$release/hashes.txt" -o "$tmp/hashes.txt"
curl -fsSL --retry 3 "$release/hashes.txt.sig" -o "$tmp/hashes.txt.sig"

if ! openssl pkeyutl -verify -rawin -pubin   -inkey "$tmp/release-signing-pub.pem"   -sigfile "$tmp/hashes.txt.sig"   -in "$tmp/hashes.txt" >/dev/null 2>&1; then
  echo 'Release manifest signature verification failed' >&2
  exit 1
fi

curl -fsSL --retry 3 "$release/install.sh" -o "$tmp/install.sh"
awk '$2=="install.sh" {print}' "$tmp/hashes.txt" >"$tmp/install.check"
[[ $(wc -l <"$tmp/install.check") == 1 ]] || {
  echo 'Signed manifest does not contain exactly one install.sh checksum' >&2
  exit 1
}
(cd "$tmp" && sha256sum -c install.check)

echo 'Signed installer verified.'
exec bash "$tmp/install.sh"
