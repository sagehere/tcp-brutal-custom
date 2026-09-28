#!/usr/bin/env bash
set -Eeuo pipefail

name='tcp-brutal-canary'
module='brutal_canary'
binary='/usr/local/bin/tbc2-canary'
ctl='/usr/local/bin/brutalctl-canary'
library='/usr/local/lib/tcp-brutal-canary'
config='/etc/tcp-brutal-canary/config.json'
data='/var/lib/tcp-brutal-canary'
mode="${1:-install}"
root="$(cd "$(dirname "$0")/.." && pwd)"

if (( EUID != 0 )); then
  echo 'Run as root' >&2
  exit 1
fi

quiesce() {
  if [[ -w /proc/net/tcp_brutal_canary/ports ]]; then
    awk '/active=1/ {split($1,a,"="); print a[2]}' /proc/net/tcp_brutal_canary/ports |
      while read -r port; do
        [[ -n "$port" ]] && printf 'del %s\n' "$port" >/proc/net/tcp_brutal_canary/ports || true
      done
  fi
  if [[ -w /proc/net/tcp_brutal_canary/rules ]]; then
    printf 'flush\n' >/proc/net/tcp_brutal_canary/rules || true
  fi
}

wait_release() {
  local waited=0
  while lsmod | grep -q '^brutal_canary '; do
    if rmmod brutal_canary 2>/dev/null; then
      return 0
    fi
    if (( waited % 30 == 0 )); then
      echo "Waiting for Canary connections to drain... (${waited}s)" >&2
    fi
    sleep 5
    waited=$((waited + 5))
  done
}

uninstall() {
  systemctl stop tcp-brutal-canary-web.service tcp-brutal-canary-manager.service 2>/dev/null || true
  systemctl disable tcp-brutal-canary-web.service tcp-brutal-canary-manager.service 2>/dev/null || true
  quiesce
  wait_release
  dkms status -m "$name" 2>/dev/null | awk -F'[/, ]+' '{print $2}' | sort -u |
    while read -r version; do
      [[ -n "$version" ]] && dkms remove "$name/$version" --all || true
    done
  rm -f "$binary" "$ctl"
  rm -f /etc/systemd/system/tcp-brutal-canary-{manager,web,uninstall}.service
  rm -rf "$library"
  for source in /usr/src/$name-*; do [[ -d "$source" ]] && rm -rf "$source"; done
  systemctl daemon-reload
  if [[ "${2:-}" == '--purge' ]]; then
    rm -rf /etc/tcp-brutal-canary "$data"
  fi
  echo 'TCP Brutal Canary removed; baseline tcp-brutal-custom was not modified.'
}

if [[ "$mode" == '--uninstall' ]]; then
  uninstall "$@"
  exit 0
fi
[[ "$mode" == 'install' ]] || { echo 'Usage: install-canary.sh [--uninstall [--purge]]' >&2; exit 2; }

for cmd in dkms gcc make go systemctl; do
  command -v "$cmd" >/dev/null || { echo "$cmd is required" >&2; exit 1; }
done
[[ -d /lib/modules/$(uname -r)/build ]] || { echo 'Matching kernel headers required' >&2; exit 1; }

getent group tcpbrutal >/dev/null || groupadd --system tcpbrutal
id tcpbrutal >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin --gid tcpbrutal tcpbrutal

short=$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo local)
version="2.1.9.canary.$short"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cd "$root"
gofiles=$(find . -maxdepth 1 -name '*.go' ! -name '*_test.go' -printf '%f ')
CGO_ENABLED=0 go build -ldflags "-X main.version=$version" -o "$tmp/tbc2-canary" $gofiles
gcc -O2 -Wall -o "$tmp/brutalctl-canary" tools/brutalctl.c

src="/usr/src/$name-$version"
rm -rf "$src"
mkdir -p "$src/tools"
cp -a Makefile brutal.h brutal_cc.c brutal_sockopt.c brutal_rules.c brutal_ports.c "$src/"
cp -a tools/brutalctl.c tools/Makefile "$src/tools/"
PACKAGE_VERSION="$version" ./scripts/mkdkmsconf.sh >"$src/dkms.conf"

if ! dkms status -m "$name" -v "$version" | grep -q 'added\|built\|installed'; then
  dkms add -m "$name" -v "$version"
fi
dkms build -m "$name" -v "$version" -k "$(uname -r)"
dkms install -m "$name" -v "$version" -k "$(uname -r)" --force

install -m 755 "$tmp/tbc2-canary" "$binary"
install -m 755 "$tmp/brutalctl-canary" "$ctl"
mkdir -p "$library" "$data"
chmod 700 "$data"
install -m 755 "$root/scripts/install-canary.sh" "$library/install-canary.sh"

cat >/etc/systemd/system/tcp-brutal-canary-manager.service <<'EOF'
[Unit]
Description=TCP Brutal Canary manager
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/sbin/modprobe brutal_canary
ExecStart=/usr/local/bin/tbc2-canary manager
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

cat >/etc/systemd/system/tcp-brutal-canary-web.service <<'EOF'
[Unit]
Description=TCP Brutal Canary web panel
After=tcp-brutal-canary-manager.service
Requires=tcp-brutal-canary-manager.service

[Service]
Type=simple
User=tcpbrutal
Group=tcpbrutal
ExecStart=/usr/local/bin/tbc2-canary web
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

cat >/etc/systemd/system/tcp-brutal-canary-uninstall.service <<'EOF'
[Unit]
Description=TCP Brutal Canary graceful uninstall

[Service]
Type=oneshot
ExecStart=/usr/local/lib/tcp-brutal-canary/install-canary.sh --uninstall
TimeoutStartSec=infinity
EOF

systemctl daemon-reload
if [[ ! -f "$config" ]]; then
  "$binary" init "${TCP_BRUTAL_CANARY_WEB_PORT:-23334}"
fi
systemctl enable tcp-brutal-canary-manager.service tcp-brutal-canary-web.service
systemctl restart tcp-brutal-canary-manager.service tcp-brutal-canary-web.service

echo "TCP Brutal Canary installed alongside baseline. Version: $version"
echo 'Baseline resources were not modified.'
