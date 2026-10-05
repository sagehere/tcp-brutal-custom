#!/usr/bin/env bash
set -Eeuo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
registration=$(sed -n '/^dkms_status=/,/^fi$/p' "$root/scripts/install.sh")
[[ -n "$registration" ]] || { echo 'DKMS registration block not found' >&2; exit 1; }

# Exercise the real installer block without touching the host DKMS tree.
dkms() {
  case "$1" in
    status)
      case "$test_state" in
        absent) return 0 ;;
        added) printf '%s/2.1.11: added\n' "$name" ;;
        built|installed)
          printf '%s/2.1.11, 6.17.0-1020-oracle, aarch64: %s\n' "$name" "$test_state"
          sleep 0.05
          printf '%s/2.1.11, 7.0.0-1012-oracle, aarch64: %s\n' "$name" "$test_state"
          ;;
        error)
          printf '%s/2.1.11: added\n' "$name"
          return 7
          ;;
      esac
      ;;
    add) printf 'ADD\n' ;;
    *) return 99 ;;
  esac
}
export -f dkms
export name=tcp-brutal-custom module_version=2.1.11 test_state

for test_state in absent added built installed error; do
  if output=$(bash -Eeuo pipefail -c "$registration"); then
    [[ "$test_state" != error ]] || { echo 'Status query failure was ignored' >&2; exit 1; }
    if [[ "$test_state" == absent ]]; then
      [[ "$output" == ADD ]] || { echo 'Missing module was not added' >&2; exit 1; }
    else
      [[ -z "$output" ]] || { echo "Duplicate add for $test_state" >&2; exit 1; }
    fi
  else
    [[ "$test_state" == error && -z "$output" ]] || { echo "Unexpected failure for $test_state" >&2; exit 1; }
  fi
done
echo 'DKMS registration checks passed (absent, added, multi-kernel built/installed, query error).'
