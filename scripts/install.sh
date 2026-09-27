#!/usr/bin/env bash
# Install only artifacts published by sagehere/tcp-brutal-custom on GitHub.
set -Eeuo pipefail

repo='https://github.com/sagehere/tcp-brutal-custom'
release="$repo/releases/latest/download"
name='tcp-brutal-custom'
module='brutal'
module_version=''
library='/usr/local/lib/tcp-brutal-custom'
binary='/usr/local/bin/tcp-brutal-custom'
config='/etc/tcp-brutal-custom/config.json'
data='/var/lib/tcp-brutal-custom'
mode="${1:-install}"
release_key="$library/release-signing-pub.pem"
release_key_fingerprint='b1a16baa2d9c68fdff5594e1261e0668f45b65253bf454b7c27025265b99bc1d'

write_embedded_release_key() {
  cat >"$1" <<'EOF'
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEA32QwT1Z9jNobN7jIIlY2KZazDgzidVQOx3/dOLp0AIs=
-----END PUBLIC KEY-----
EOF
}

verify_release_key() {
  local actual
  actual=$(openssl pkey -pubin -in "$1" -outform DER 2>/dev/null | openssl dgst -sha256 | awk '{print $2}')
  [[ "$actual" == "$release_key_fingerprint" ]] || {
    echo "Release signing key fingerprint mismatch" >&2
    exit 1
  }
}

if (( EUID != 0 )); then echo 'Run as root' >&2; exit 1; fi

check_host() {
  source /etc/os-release
  case "$ID:$VERSION_ID" in
    debian:12|debian:13|ubuntu:22.04|ubuntu:24.04) ;;
    *) echo "Unsupported distribution: $ID $VERSION_ID" >&2; exit 1 ;;
  esac
  command -v systemctl >/dev/null || { echo 'systemd required' >&2; exit 1; }
  [[ $(stat -fc %T /sys/fs/cgroup) == cgroup2fs ]] || { echo 'cgroup v2 required' >&2; exit 1; }
  case "$(uname -m)" in x86_64) arch=amd64;; aarch64) arch=arm64;; *) echo 'Unsupported CPU' >&2; exit 1;; esac
  command -v curl >/dev/null || { echo 'curl required' >&2; exit 1; }
  command -v sha256sum >/dev/null || { echo 'sha256sum required' >&2; exit 1; }
  command -v ss >/dev/null || { echo 'iproute2 ss required' >&2; exit 1; }
  if ! command -v openssl >/dev/null; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y openssl
  fi
}

write_units() {
  cat >/etc/systemd/system/tcp-brutal-custom-manager.service <<'EOF'
[Unit]
Description=TCP Brutal Custom manager
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/sbin/modprobe brutal
ExecStart=/usr/local/bin/tcp-brutal-custom manager
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
  cat >/etc/systemd/system/tcp-brutal-custom-web.service <<'EOF'
[Unit]
Description=TCP Brutal Custom web panel
After=tcp-brutal-custom-manager.service
Requires=tcp-brutal-custom-manager.service

[Service]
Type=simple
User=tcpbrutal
Group=tcpbrutal
ExecStart=/usr/local/bin/tcp-brutal-custom web
Restart=on-failure
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=multi-user.target
EOF
  cat >/etc/systemd/system/tcp-brutal-custom-update.service <<'EOF'
[Unit]
Description=TCP Brutal Custom maintenance upgrade
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/lib/tcp-brutal-custom/install.sh --update
TimeoutStartSec=900
EOF
  systemctl daemon-reload
}

record_status() {
  mkdir -p "$data"
  printf '{"state":"%s","detail":"%s","time":%s}\n' "$1" "$2" "$(date +%s)" >"$data/update.json"
  chmod 600 "$data/update.json"
}

uninstall() {
  systemctl stop tcp-brutal-custom-web.service tcp-brutal-custom-manager.service 2>/dev/null || true
  systemctl disable tcp-brutal-custom-web.service tcp-brutal-custom-manager.service 2>/dev/null || true
  if [[ -e /proc/net/tcp_brutal/ports ]]; then
    awk '/active=1/ {split($1,a,"=");print a[2]}' /proc/net/tcp_brutal/ports | while read -r port; do
      [[ -n "$port" ]] && ss -K state established "( sport = :$port )" >/dev/null 2>&1 || true
    done
  fi
  if lsmod | grep -q '^brutal '; then
    if ! rmmod brutal; then
      systemctl start tcp-brutal-custom-manager.service tcp-brutal-custom-web.service 2>/dev/null || true
      echo 'Module in use; installation retained for recovery' >&2
      exit 1
    fi
  fi
  dkms status -m "$name" 2>/dev/null | awk -F'[/, ]+' '{print $2}' | sort -u | while read -r version; do
    [[ -n "$version" ]] && dkms remove "$name/$version" --all || true
  done
  rm -f "$binary" /usr/local/bin/brutalctl
  rm -f /etc/systemd/system/tcp-brutal-custom-{manager,web,update}.service
  rm -rf "$library"
  for source in /usr/src/$name-*; do [[ -d "$source" ]] && rm -rf "$source"; done
  if [[ -f "$data/upstream/module-path" && -f "$data/upstream/brutal.ko" ]]; then
    original=$(cat "$data/upstream/module-path")
    cp -a "$data/upstream/brutal.ko" "$original"
    depmod -a
    modprobe brutal || true
    if [[ -f "$data/upstream/brutalctl" ]]; then install -m 755 "$data/upstream/brutalctl" /usr/local/bin/brutalctl; fi
    if [[ -f "$data/upstream/rules" && -e /proc/net/tcp_brutal/rules ]]; then
      while read -r line; do
        dst=$(sed -n 's/.*dst=\([^ ]*\).*/\1/p' <<<"$line")
        rate=$(sed -n 's/.*rate=\([^ ]*\).*/\1/p' <<<"$line")
        gain=$(sed -n 's/.*gain=\([^ ]*\).*/\1/p' <<<"$line")
        [[ -n "$dst" && -n "$rate" && -n "$gain" ]] && printf 'add %s rate=%s gain=%s\n' "$dst" "$rate" "$gain" >/proc/net/tcp_brutal/rules || true
      done <"$data/upstream/rules"
    fi
  fi
  systemctl daemon-reload
  if [[ "${2:-}" == '--purge' ]]; then rm -rf /etc/tcp-brutal-custom "$data"; fi
  echo 'TCP Brutal Custom removed.'
}

case "$mode" in
  --uninstall) uninstall "$@"; exit 0;;
  install|--update) ;;
  *) echo 'Usage: install.sh [--update|--uninstall [--purge]]' >&2; exit 2;;
esac
check_host
if [[ "$mode" == '--update' ]]; then trap 'record_status failed staging' ERR; fi
if [[ ! -f "$config" ]]; then
  panel_port="${TCP_BRUTAL_WEB_PORT:-23333}"
  [[ "$panel_port" =~ ^[0-9]+$ ]] && ((panel_port >= 1024 && panel_port <= 65535)) || { echo 'Invalid panel port' >&2; exit 1; }
  if ss -ltn "( sport = :$panel_port )" | grep -q LISTEN; then echo "Panel port $panel_port is already in use" >&2; exit 1; fi
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Updates trust the key already pinned on the machine. Fresh installs use the
# embedded bootstrap key; operators should verify its fingerprint out-of-band.
if [[ "$mode" == '--update' && -f "$release_key" ]]; then
  cp -a "$release_key" "$tmp/trusted-release-key.pem"
else
  write_embedded_release_key "$tmp/trusted-release-key.pem"
fi
verify_release_key "$tmp/trusted-release-key.pem"

curl -fsSL --retry 3 "$release/hashes.txt" -o "$tmp/hashes.txt"
curl -fsSL --retry 3 "$release/hashes.txt.sig" -o "$tmp/hashes.txt.sig"
if ! openssl pkeyutl -verify -rawin -pubin -inkey "$tmp/trusted-release-key.pem"     -sigfile "$tmp/hashes.txt.sig" -in "$tmp/hashes.txt" >/dev/null 2>&1; then
  echo 'Release manifest signature verification failed' >&2
  exit 1
fi

curl -fsSL --retry 3 "$release/$name-linux-$arch" -o "$tmp/$name-linux-$arch"
curl -fsSL --retry 3 "$release/$name.dkms.tar.gz" -o "$tmp/$name.dkms.tar.gz"
curl -fsSL --retry 3 "$release/install.sh" -o "$tmp/install.sh"
for file in "$name-linux-$arch" "$name.dkms.tar.gz" install.sh; do
  awk -v file="$file" '$2==file {print}' "$tmp/hashes.txt" >"$tmp/check"
  [[ $(wc -l <"$tmp/check") == 1 ]] || { echo "Missing checksum for $file" >&2; exit 1; }
  (cd "$tmp" && sha256sum -c check)
done
dkms_conf_member=$(tar tzf "$tmp/$name.dkms.tar.gz" | awk '$0=="dkms_source_tree/dkms.conf" || $0=="./dkms_source_tree/dkms.conf" {print; exit}')
[[ -n "$dkms_conf_member" ]] || { echo 'Invalid DKMS package: dkms.conf missing' >&2; exit 1; }
case "$dkms_conf_member" in
  dkms_source_tree/dkms.conf) dkms_strip=1 ;;
  ./dkms_source_tree/dkms.conf) dkms_strip=2 ;;
  *) echo 'Invalid DKMS package layout' >&2; exit 1 ;;
esac
module_version=$(tar xOzf "$tmp/$name.dkms.tar.gz" "$dkms_conf_member" | sed -n 's/^PACKAGE_VERSION="\([^"]*\)"$/\1/p')
[[ "$module_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid DKMS package version' >&2; exit 1; }

apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y dkms gcc make iproute2 "linux-headers-$(uname -r)"
[[ -d "/lib/modules/$(uname -r)/build" ]] || { echo 'Matching kernel headers unavailable' >&2; exit 1; }
if ! getent group tcpbrutal >/dev/null; then groupadd --system tcpbrutal; fi
if ! id tcpbrutal >/dev/null 2>&1; then useradd --system --no-create-home --shell /usr/sbin/nologin --gid tcpbrutal tcpbrutal; fi

mkdir -p "$data" "$library" "/usr/src/$name-$module_version"
chmod 700 "$data"
if [[ ! -f "$release_key" ]]; then
  install -m 644 "$tmp/trusted-release-key.pem" "$release_key"
else
  verify_release_key "$release_key"
fi
if [[ ! -f "$library/module-version" && ! -d "$data/upstream" ]]; then
  original=$(modinfo -n brutal 2>/dev/null || true)
  if [[ -n "$original" && -f "$original" ]]; then
    mkdir -m 700 "$data/upstream"
    cp -a "$original" "$data/upstream/brutal.ko"
    printf '%s\n' "$original" >"$data/upstream/module-path"
    [[ ! -f /usr/local/bin/brutalctl ]] || cp -a /usr/local/bin/brutalctl "$data/upstream/brutalctl"
    [[ ! -e /proc/net/tcp_brutal/rules ]] || cp -a /proc/net/tcp_brutal/rules "$data/upstream/rules"
    ip route show proto 233 >"$data/upstream/routes-v4" 2>/dev/null || true
    ip -6 route show proto 233 >"$data/upstream/routes-v6" 2>/dev/null || true
  fi
fi
tar xzf "$tmp/$name.dkms.tar.gz" -C "/usr/src/$name-$module_version" --strip-components="$dkms_strip"
[[ -f "/usr/src/$name-$module_version/dkms.conf" && -f "/usr/src/$name-$module_version/Makefile" ]] || {
  echo 'Invalid DKMS source package layout after extraction' >&2
  exit 1
}
if ! dkms status -m "$name" -v "$module_version" | grep -q 'added\|built\|installed'; then
  dkms add -m "$name" -v "$module_version"
fi
dkms build -m "$name" -v "$module_version" -k "$(uname -r)"
cc -O2 -Wall -o "$tmp/brutalctl" "/usr/src/$name-$module_version/tools/brutalctl.c"

if [[ "$mode" == '--update' ]]; then
  record_status running 'staged'
fi

old_module=$(modinfo -n "$module" 2>/dev/null || true)
if [[ -n "$old_module" && -f "$old_module" ]]; then cp -a "$old_module" "$tmp/old-module"; fi
if [[ -f "$binary" ]]; then cp -a "$binary" "$tmp/old-binary"; fi
if [[ -f "$library/install.sh" ]]; then cp -a "$library/install.sh" "$tmp/old-installer"; fi
if [[ -f /usr/local/bin/brutalctl ]]; then cp -a /usr/local/bin/brutalctl "$tmp/old-brutalctl"; fi
systemctl stop tcp-brutal-custom-web.service tcp-brutal-custom-manager.service 2>/dev/null || true
if [[ "$mode" == '--update' && -e /proc/net/tcp_brutal/ports ]]; then
  awk '/active=1/ {split($1,a,"=");print a[2]}' /proc/net/tcp_brutal/ports | while read -r port; do
    [[ -n "$port" ]] && ss -K state established "( sport = :$port )" >/dev/null 2>&1 || true
  done
fi
if lsmod | grep -q '^brutal '; then
  if ! rmmod brutal; then
    record_status failed 'module-busy'
    systemctl start tcp-brutal-custom-manager.service tcp-brutal-custom-web.service 2>/dev/null || true
    echo 'Module still in use; upgrade was not activated' >&2
    exit 1
  fi
fi

rollback() {
  trap - ERR
  systemctl stop tcp-brutal-custom-web.service tcp-brutal-custom-manager.service 2>/dev/null || true
  if lsmod | grep -q '^brutal '; then rmmod brutal || true; fi
  [[ -f "$tmp/old-binary" ]] && install -m 755 "$tmp/old-binary" "$binary"
  [[ -f "$tmp/old-brutalctl" ]] && install -m 755 "$tmp/old-brutalctl" /usr/local/bin/brutalctl
  if [[ -f "$tmp/old-installer" ]]; then
    install -m 755 "$tmp/old-installer" "$library/install.sh.old"
    mv -f "$library/install.sh.old" "$library/install.sh"
  fi
  if [[ -n "$old_module" && -f "$tmp/old-module" ]]; then
    cp -a "$tmp/old-module" "$old_module"
    depmod -a
    modprobe brutal || true
  fi
  record_status failed 'rolled-back'
  systemctl start tcp-brutal-custom-manager.service tcp-brutal-custom-web.service 2>/dev/null || true
}
trap 'rollback' ERR
dkms install -m "$name" -v "$module_version" -k "$(uname -r)" --force
install -m 755 "$tmp/$name-linux-$arch" "$binary"
install -m 755 "$tmp/brutalctl" /usr/local/bin/brutalctl
install -m 755 "$tmp/install.sh" "$library/install.sh.new"
mv -f "$library/install.sh.new" "$library/install.sh"
printf '%s\n' "$module_version" >"$library/module-version"
if [[ ! -f "$config" ]]; then
  "$binary" init "$panel_port"
fi
write_units
modprobe brutal
if [[ "$mode" == 'install' ]]; then systemctl enable tcp-brutal-custom-manager.service tcp-brutal-custom-web.service; fi
systemctl start tcp-brutal-custom-manager.service tcp-brutal-custom-web.service
trap - ERR
record_status complete 'active'
panel_port=$(sed -n 's/.*"web_port": \([0-9]*\).*/\1/p' "$config" | head -1)
echo "TCP Brutal Custom installed. Panel: http://$(hostname -I | awk '{print $1}'):$panel_port"
