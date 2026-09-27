#!/usr/bin/env bash
set -Eeuo pipefail

repo='https://github.com/sagehere/tcp-brutal-custom'
library='/usr/local/lib/tcp-brutal-custom'
release_key="$library/release-signing-pub.pem"
new_fingerprint='249a5abded1a497f8fe67f4cf5cd8e47d127b9cee2d9b1eac23042b7edfeceff'
old_fingerprint='b1a16baa2d9c68fdff5594e1261e0668f45b65253bf454b7c27025265b99bc1d'
accept="${TCP_BRUTAL_ACCEPT_RELEASE_KEY:-}"

if (( EUID != 0 )); then
  echo 'Run as root.' >&2
  exit 1
fi
command -v openssl >/dev/null || { echo 'openssl required' >&2; exit 1; }
command -v curl >/dev/null || { echo 'curl required' >&2; exit 1; }
command -v sha256sum >/dev/null || { echo 'sha256sum required' >&2; exit 1; }

if [[ "$accept" != "$new_fingerprint" ]]; then
  cat >&2 <<EOF
Refusing to rotate the release trust root without explicit approval.

Old fingerprint:
  $old_fingerprint

New fingerprint:
  $new_fingerprint

Independently verify the new fingerprint, then rerun with:
  TCP_BRUTAL_ACCEPT_RELEASE_KEY=$new_fingerprint
EOF
  exit 2
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

cat >"$tmp/new-release-key.pem" <<'EOF'
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAaop0CqyMxEmosNEtgEWgidNFVh/xaIE4uzAJDqfCqeA=
-----END PUBLIC KEY-----
EOF

actual=$(openssl pkey -pubin -in "$tmp/new-release-key.pem" -outform DER 2>/dev/null |
  openssl dgst -sha256 | awk '{print $2}')
[[ "$actual" == "$new_fingerprint" ]] || { echo 'Embedded new key fingerprint mismatch' >&2; exit 1; }

if [[ -f "$release_key" ]]; then
  current=$(openssl pkey -pubin -in "$release_key" -outform DER 2>/dev/null |
    openssl dgst -sha256 | awk '{print $2}')
  case "$current" in
    "$old_fingerprint") ;;
    "$new_fingerprint") echo 'Release key is already migrated.'; exit 0 ;;
    *) echo "Refusing to replace unknown pinned release key: $current" >&2; exit 1 ;;
  esac
else
  echo 'No pinned release key found; use the normal verified installer instead.' >&2
  exit 1
fi

release="$repo/releases/latest/download"
curl -fsSL --retry 3 "$release/hashes.txt" -o "$tmp/hashes.txt"
curl -fsSL --retry 3 "$release/hashes.txt.sig" -o "$tmp/hashes.txt.sig"
openssl pkeyutl -verify -rawin -pubin   -inkey "$tmp/new-release-key.pem"   -sigfile "$tmp/hashes.txt.sig"   -in "$tmp/hashes.txt" >/dev/null

curl -fsSL --retry 3 "$release/install.sh" -o "$tmp/install.sh"
awk '$2=="install.sh" {print}' "$tmp/hashes.txt" >"$tmp/install.check"
[[ $(wc -l <"$tmp/install.check") == 1 ]] || { echo 'Signed manifest has invalid install.sh entry' >&2; exit 1; }
(cd "$tmp" && sha256sum -c install.check)

install -d -m 755 "$library"
install -m 644 "$tmp/new-release-key.pem" "$library/release-signing-pub.pem.new"
mv -f "$library/release-signing-pub.pem.new" "$release_key"
install -m 755 "$tmp/install.sh" "$library/install.sh.new"
mv -f "$library/install.sh.new" "$library/install.sh"

echo "Release trust root migrated to $new_fingerprint"
echo 'You may now run: sudo tbc2 update'
