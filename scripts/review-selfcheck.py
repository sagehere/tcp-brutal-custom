#!/usr/bin/env python3
"""Exercise isolated self-check success/failure and verify BPF/rule cleanup."""
import argparse
import ctypes
import errno
import json
import os
from pathlib import Path
import platform
import subprocess
import time


def bpf_ids(command):
    number = {'aarch64': 280, 'x86_64': 321}.get(platform.machine())
    if number is None:
        raise RuntimeError('BPF syscall number unavailable for this architecture')
    libc = ctypes.CDLL(None, use_errno=True)
    result = []
    start = 0
    while True:
        attr = (ctypes.c_uint32 * 3)(start, 0, 0)
        if libc.syscall(number, command, ctypes.byref(attr), ctypes.sizeof(attr)):
            error = ctypes.get_errno()
            if error == errno.ENOENT:
                return result
            raise OSError(error, os.strerror(error))
        start = attr[1]
        result.append(start)


def snapshot(ports):
    keys = {'port', 'active', 'rate', 'gain', 'id', 'compensation_cap_percent'}
    return {'programs': bpf_ids(11), 'maps': bpf_ids(12),
            'active_rules': sorted(' '.join(token for token in line.split() if token.split('=')[0] in keys)
                                   for line in ports.read_text().splitlines() if 'active=1' in line)}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--success', required=True)
    parser.add_argument('--failure', required=True, help='isolated build naming a nonexistent algorithm')
    parser.add_argument('--ports', default='/proc/net/tcp_brutal_review/ports')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    records = []
    try:
        for _ in range(3):
            for binary, expected in ((args.success, 0), (args.failure, 1)):
                before = snapshot(Path(args.ports))
                run = subprocess.run([str(Path(binary).resolve()), 'probe-bpf'], capture_output=True,
                                     text=True, timeout=15)
                after = snapshot(Path(args.ports))
                # Kernel BPF frees can be deferred through RCU/workqueues.
                deadline = time.monotonic() + 3
                while before != after and time.monotonic() < deadline:
                    time.sleep(.05)
                    after = snapshot(Path(args.ports))
                records.append({'binary': binary, 'exit_code': run.returncode,
                                'stdout': run.stdout, 'stderr': run.stderr,
                                'before': before, 'after': after})
                args.output.joinpath('results.json').write_text(json.dumps(records, indent=2))
                assert run.returncode == expected, records[-1]
                assert before == after, 'BPF object or active rule remained after self-check'
        args.output.joinpath('exit-status.json').write_text(json.dumps({'exit_code': 0}))
    except BaseException as error:
        args.output.joinpath('exit-status.json').write_text(json.dumps({'exit_code': 1, 'error': str(error)}))
        raise


if __name__ == '__main__':
    main()
