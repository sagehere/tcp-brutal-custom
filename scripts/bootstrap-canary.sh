#!/usr/bin/env bash
set -Eeuo pipefail

repo_url="${TCP_BRUTAL_CANARY_REPO:-https://github.com/sagehere/tcp-brutal-custom.git}"
ref="${TCP_BRUTAL_CANARY_REF:-feature/canary-ab-reporting}"
web_port="${TCP_BRUTAL_CANARY_WEB_PORT:-23334}"
keep_source="${TCP_BRUTAL_CANARY_KEEP_SOURCE:-0}"

die() { echo "ERROR: $*" >&2; exit 1; }
note() { printf '%s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

if (( EUID != 0 )); then
  die "run this installer as root"
fi

[[ -r /etc/os-release ]] || die "/etc/os-release is missing"
. /etc/os-release
case "${ID:-}" in
  debian)
    case "${VERSION_ID:-}" in 12|13) ;; *) die "unsupported Debian release: ${VERSION_ID:-unknown}" ;; esac
    ;;
  ubuntu)
    case "${VERSION_ID:-}" in 22.04|24.04) ;; *) die "unsupported Ubuntu release: ${VERSION_ID:-unknown}" ;; esac
    ;;
  *) die "unsupported OS: ${ID:-unknown}; supported: Debian 12/13, Ubuntu 22.04/24.04" ;;
esac

arch="$(dpkg --print-architecture 2>/dev/null || true)"
case "$arch" in amd64|arm64) ;; *) die "unsupported architecture: ${arch:-unknown}; supported: amd64, arm64" ;; esac

kernel="$(uname -r)"
[[ -r /sys/fs/cgroup/cgroup.controllers ]] || die "cgroup v2 is required"

baseline_module_before=0
[[ -d /sys/module/brutal ]] && baseline_module_before=1
baseline_cc_before="$(cat /proc/sys/net/ipv4/tcp_available_congestion_control 2>/dev/null || true)"
baseline_tbc2_before="$(command -v tbc2 2>/dev/null || true)"
baseline_tbc2_sha_before=""
[[ -n "$baseline_tbc2_before" && -f "$baseline_tbc2_before" ]] && baseline_tbc2_sha_before="$(sha256sum "$baseline_tbc2_before" | awk '{print $1}')"
baseline_cfg_before=""
if [[ -d /etc/tcp-brutal-custom ]]; then
  baseline_cfg_before="$(find /etc/tcp-brutal-custom -xdev -type f -print0 2>/dev/null | sort -z | xargs -0r sha256sum | sha256sum | awk '{print $1}')"
fi
baseline_units_before="$(systemctl list-unit-files 'tcp-brutal-custom*' --no-legend 2>/dev/null || true)"
baseline_active_before="$(systemctl list-units 'tcp-brutal-custom*' --state=active --no-legend 2>/dev/null || true)"

note "TCP Brutal Canary bootstrap"
note "  OS:      ${PRETTY_NAME:-$ID}"
note "  arch:    $arch"
note "  kernel:  $kernel"
note "  ref:     $ref"

export DEBIAN_FRONTEND=noninteractive
need_apt=0
for c in git dkms gcc make go systemctl sha256sum; do
  have "$c" || need_apt=1
done
[[ -d "/lib/modules/$kernel/build" ]] || need_apt=1

if (( need_apt )); then
  have apt-get || die "apt-get is required to install build dependencies"
  apt-get update
  packages=(git dkms build-essential golang-go ca-certificates)
  if ! [[ -d "/lib/modules/$kernel/build" ]]; then
    packages+=("linux-headers-$kernel")
  fi
  apt-get install -y "${packages[@]}" || die "failed to install dependencies; ensure matching headers exist for $kernel"
fi

for c in git dkms gcc make go systemctl sha256sum; do
  have "$c" || die "required command is still missing after dependency install: $c"
done
[[ -d "/lib/modules/$kernel/build" ]] || die "matching kernel headers are unavailable: /lib/modules/$kernel/build"

work="$(mktemp -d /tmp/tcp-brutal-canary-bootstrap.XXXXXX)"
cleanup() {
  if [[ "$keep_source" != 1 ]]; then rm -rf "$work"; else note "source kept at: $work"; fi
}
trap cleanup EXIT

git -C "$work" init -q
git -C "$work" remote add origin "$repo_url"
if ! git -C "$work" fetch -q --depth 1 origin "$ref"; then
  die "cannot fetch TCP_BRUTAL_CANARY_REF=$ref from $repo_url"
fi
git -C "$work" checkout -q --detach FETCH_HEAD
commit="$(git -C "$work" rev-parse HEAD)"
short="$(git -C "$work" rev-parse --short=12 HEAD)"
note "  commit:  $commit"

[[ -x "$work/scripts/install-canary.sh" ]] || die "scripts/install-canary.sh is missing or not executable at $ref"

# Hard isolation guard: the Canary installer must target Canary resources.
grep -q "module='brutal_canary'" "$work/scripts/install-canary.sh" || die "unexpected installer: brutal_canary module declaration missing"
grep -q "binary='/usr/local/bin/tbc2-canary'" "$work/scripts/install-canary.sh" || die "unexpected installer: tbc2-canary declaration missing"
grep -q "name='tcp-brutal-canary'" "$work/scripts/install-canary.sh" || die "unexpected installer: Canary DKMS package declaration missing"

TCP_BRUTAL_CANARY_WEB_PORT="$web_port" "$work/scripts/install-canary.sh"

mkdir -p /etc/tcp-brutal-canary
installed_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
cat > /etc/tcp-brutal-canary/build-info.json <<JSON
{
  "branch": "$(json_escape "$ref")",
  "commit": "$(json_escape "$commit")",
  "short_commit": "$(json_escape "$short")",
  "installed_at": "$(json_escape "$installed_at")",
  "kernel": "$(json_escape "$kernel")",
  "arch": "$(json_escape "$arch")",
  "os": "$(json_escape "${PRETTY_NAME:-$ID}")"
}
JSON
chmod 600 /etc/tcp-brutal-canary/build-info.json

fail=0
ok() { printf '  %-26s OK\n' "$1"; }
bad() { printf '  %-26s FAIL%s\n' "$1" "${2:+ - $2}"; fail=1; }

note
note "Automatic acceptance checks"

if modprobe brutal_canary; then ok "brutal_canary module"; else bad "brutal_canary module"; fi
if grep -qw brutal_adaptive /proc/sys/net/ipv4/tcp_available_congestion_control 2>/dev/null; then ok "brutal_adaptive TCP CC"; else bad "brutal_adaptive TCP CC"; fi
if [[ -d /proc/net/tcp_brutal_canary ]]; then ok "Canary procfs"; else bad "Canary procfs"; fi
if systemctl is-active --quiet tcp-brutal-canary-manager.service; then ok "Canary manager"; else bad "Canary manager"; fi
web_ready=0
for _ in $(seq 1 15); do
  if systemctl is-active --quiet tcp-brutal-canary-web.service; then web_ready=1; break; fi
  sleep 1
done
if (( web_ready )); then ok "Canary web"; else bad "Canary web"; fi
if /usr/local/bin/tbc2-canary probe-bpf >/tmp/tcp-brutal-canary-probe.log 2>&1; then
  ok "BPF selector probe"
else
  bad "BPF selector probe" "$(tail -n 1 /tmp/tcp-brutal-canary-probe.log 2>/dev/null || true)"
fi
rm -f /tmp/tcp-brutal-canary-probe.log

if (( baseline_module_before )); then
  if [[ -d /sys/module/brutal ]]; then ok "baseline brutal retained"; else bad "baseline brutal retained"; fi
else
  ok "baseline module untouched"
fi

baseline_cc_after="$(cat /proc/sys/net/ipv4/tcp_available_congestion_control 2>/dev/null || true)"
if [[ "$baseline_cc_before" == *" brutal"* || "$baseline_cc_before" == brutal* ]]; then
  if grep -qw brutal <<<"$baseline_cc_after"; then ok "baseline TCP CC retained"; else bad "baseline TCP CC retained"; fi
else
  ok "baseline TCP CC untouched"
fi

if [[ -n "$baseline_tbc2_before" ]]; then
  baseline_tbc2_after="$(command -v tbc2 2>/dev/null || true)"
  baseline_tbc2_sha_after=""
  [[ -n "$baseline_tbc2_after" && -f "$baseline_tbc2_after" ]] && baseline_tbc2_sha_after="$(sha256sum "$baseline_tbc2_after" | awk '{print $1}')"
  if [[ "$baseline_tbc2_before" == "$baseline_tbc2_after" && "$baseline_tbc2_sha_before" == "$baseline_tbc2_sha_after" ]]; then
    ok "baseline tbc2 unchanged"
  else
    bad "baseline tbc2 unchanged"
  fi
else
  ok "baseline tbc2 absent/untouched"
fi

if [[ -d /etc/tcp-brutal-custom ]]; then
  baseline_cfg_after="$(find /etc/tcp-brutal-custom -xdev -type f -print0 2>/dev/null | sort -z | xargs -0r sha256sum | sha256sum | awk '{print $1}')"
  if [[ "$baseline_cfg_before" == "$baseline_cfg_after" ]]; then ok "baseline config unchanged"; else bad "baseline config unchanged"; fi
else
  [[ -z "$baseline_cfg_before" ]] && ok "baseline config untouched" || bad "baseline config retained"
fi

baseline_units_after="$(systemctl list-unit-files 'tcp-brutal-custom*' --no-legend 2>/dev/null || true)"
baseline_active_after="$(systemctl list-units 'tcp-brutal-custom*' --state=active --no-legend 2>/dev/null || true)"
if [[ "$baseline_units_before" == "$baseline_units_after" ]]; then ok "baseline units unchanged"; else bad "baseline units unchanged"; fi
if [[ "$baseline_active_before" == "$baseline_active_after" ]]; then ok "baseline service state"; else bad "baseline service state"; fi

note
note "Installed build"
note "  requested ref: $ref"
note "  commit:        $commit"
note "  build info:    /etc/tcp-brutal-canary/build-info.json"
note "  panel:         port $web_port"
note "  CLI:           /usr/local/bin/tbc2-canary"

if (( fail )); then
  die "installation completed but one or more acceptance checks failed"
fi
note "Canary installation and isolation checks passed."