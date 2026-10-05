#!/usr/bin/env bash
# Requires the isolated baseline/review modules; touches only the test network.
set -Eeuo pipefail -o noclobber
[[ $# == 1 ]] || { echo "usage: $0 NEW_OUTPUT_DIRECTORY" >&2; exit 2; }
[[ ! -e "$1" && ! -L "$1" ]] || { echo 'output already exists' >&2; exit 2; }
script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
python3 "$script_dir/review-benchmark.py" --output "$1" --quick --seconds 4 --warmup 1 --repeats 1 --algorithms cubic,bbr --continue-on-error >"$1.log" 2>&1 &
benchmark_pid=$!
trap 'kill -TERM "$benchmark_pid" 2>/dev/null || true; wait "$benchmark_pid" 2>/dev/null || true' EXIT
killed=false
for attempt in $(seq 1 80); do
  # Only a one-off server created by this benchmark process, never a global PID.
  target_pid=$(ps --ppid "$benchmark_pid" -o pid=,args= | awk '$2=="iperf3" && $3=="-s" {print $1; exit}') || target_pid=''
  if [[ -n "$target_pid" ]] && kill -TERM "$target_pid"; then killed=true; break; fi
  sleep .05
done
[[ $killed == true ]]
status=0
wait "$benchmark_pid" || status=$?
[[ $status == 1 ]]
python3 - "$1" <<'PY'
import json
from pathlib import Path
import sys
p = Path(sys.argv[1])
assert json.loads((p / 'exit-status.json').read_text())['exit_code'] == 1
assert json.loads((p / 'cleanup-status.json').read_text())['cleaned']
assert len(json.loads((p / 'results.json').read_text())) == 1
failed = json.loads((p / 'failures.json').read_text())
assert len(failed) == 1 and 'Connection refused' in failed[0]['error']
assert (p / (failed[0]['label'] + '.stdout.txt')).exists()
print('failure evidence and continuation verified')
PY
! ip link show brv_tx >/dev/null 2>&1
! ip netns list | grep -Eq '^(brv_client|brv_router)( |$)'
