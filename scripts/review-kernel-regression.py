#!/usr/bin/env python3
"""Functional kernel checks in a disposable veth namespace, not a WAN benchmark.

Run after the performance comparison (not concurrently) with brutal_review loaded.
Only brk_tx/brk_rx/brk_client and the module's temporary 54002 rule are modified.
"""
import argparse
import json
import os
import select
import signal
import socket
import struct
import subprocess
import sys
import time
from pathlib import Path


def run(*args, check=True):
    return subprocess.run(args, check=check, capture_output=True, text=True, timeout=10)


def receive(plan):
    sockets, counts, ticks = {}, {}, []
    start = time.monotonic()
    while time.monotonic() - start < plan['seconds']:
        elapsed = time.monotonic() - start
        for index in range(plan['flows']):
            if index not in counts and (index < plan['initial'] or elapsed >= 10):
                connection = socket.create_connection(('172.31.238.1', 54002), timeout=3)
                connection.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 32768)
                connection.setblocking(False)
                sockets[index], counts[index] = connection, 0
        if plan['close'] and elapsed >= 15 and 0 in sockets:
            sockets.pop(0).close()
        reading = [s for i, s in sockets.items() if not (plan['pause'] and i == 0 and 8 <= elapsed < 11)]
        ready, _, _ = select.select(reading, [], [], .02)
        for connection in ready:
            try:
                data = connection.recv(65536)
                if not data:
                    continue
                index = next(i for i, s in sockets.items() if s == connection)
                counts[index] += len(data)
            except BlockingIOError:
                pass
        second = int(elapsed)
        if len(ticks) <= second:
            ticks.append({'second': second, 'bytes': sum(counts.values()), 'flows': dict(counts)})
    for connection in sockets.values():
        connection.close()
    print(json.dumps({'bytes': sum(counts.values()), 'flows': counts, 'ticks': ticks}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error('root required')
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=False)
    namespace, tx, rx = 'brk_client', 'brk_tx', 'brk_rx'
    ports = Path('/proc/net/tcp_brutal_review/ports')
    if not ports.exists() or 'port=54002 active=1' in ports.read_text():
        raise RuntimeError('review module missing or test port already managed')
    if namespace in {x.split()[0] for x in run('ip', 'netns', 'list').stdout.splitlines()} or run('ip', 'link', 'show', tx, check=False).returncode == 0:
        raise RuntimeError('test network already exists; refusing to overwrite it')
    owned_namespace = owned_link = False
    results = []
    def rule(rate=5):
        ports.write_text(f'add 54002 rate={rate * 125000} gain=20 compensation_cap_percent=100\n')
    try:
        run('ip', 'netns', 'add', namespace); owned_namespace = True
        run('ip', 'link', 'add', tx, 'type', 'veth', 'peer', 'name', rx); owned_link = True
        run('ip', 'link', 'set', rx, 'netns', namespace)
        run('ip', 'addr', 'add', '172.31.238.1/30', 'dev', tx)
        run('ip', 'link', 'set', tx, 'up')
        run('ip', '-n', namespace, 'addr', 'add', '172.31.238.2/30', 'dev', rx)
        run('ip', '-n', namespace, 'link', 'set', rx, 'up')
        run('ip', '-n', namespace, 'link', 'set', 'lo', 'up')
        output.joinpath('environment.json').write_text(json.dumps({'kernel': run('uname', '-r').stdout.strip(), 'host_qdiscs': run('tc', 'qdisc', 'show').stdout}, indent=2))
        for cap in (0, 99, 126):
            try:
                ports.write_text(f'add 54002 rate=625000 gain=20 compensation_cap_percent={cap}\n')
                raise AssertionError('invalid kernel compensation cap accepted')
            except OSError:
                pass
        ports.write_text('add 54002 rate=625000 gain=20 compensation_cap_percent=110\n')
        ports.write_text('add 54002 rate=1250000 gain=20\n')
        assert any('port=54002 active=1' in line and 'compensation_cap_percent=110' in line for line in ports.read_text().splitlines()), 'legacy kernel update reset compensation cap'
        ports.write_text('del 54002\n')
        # Verify the unchanged 12/20-byte application ABI on an unruled socket.
        with socket.socket() as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(('172.31.238.1', 54002)); listener.listen(); listener.settimeout(3)
            abi_plan = {'seconds': 2, 'flows': 1, 'initial': 1, 'close': False, 'pause': False}
            abi_client = subprocess.Popen(['ip', 'netns', 'exec', namespace, sys.executable, str(Path(__file__).resolve()), '--receiver', json.dumps(abi_plan)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                with listener.accept()[0] as connection:
                    connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_CONGESTION, b'brutal_review')
                    connection.setsockopt(socket.IPPROTO_TCP, 23301, struct.pack('=QI', 625000, 20))
                    assert struct.unpack('=QI', connection.getsockopt(socket.IPPROTO_TCP, 23301, 12)) == (625000, 20)
                    connection.setsockopt(socket.IPPROTO_TCP, 23301, struct.pack('=QIQ', 625000, 20, 7777))
                    assert struct.unpack('=QIQ', connection.getsockopt(socket.IPPROTO_TCP, 23301, 20)) == (625000, 20, 7777)
                    connection.sendall(b'abi-check')
                stdout, stderr = abi_client.communicate(timeout=5)
                assert abi_client.returncode == 0 and json.loads(stdout)['bytes'] == 9, stderr
            finally:
                if abi_client.poll() is None:
                    abi_client.terminate(); abi_client.wait(timeout=5)
        cases = [
            ('single', 1, False, False, False, False, False),
            ('members-exit', 8, True, False, False, False, False),
            ('rate-window-recovery', 4, False, True, True, False, False),
            ('gso-fq', 4, False, False, False, True, False),
            ('gso-internal', 4, False, False, False, True, True),
            ('old-new-groups', 3, False, False, False, False, False),
        ]
        for name, flows, close, pause, change, offload, internal in cases:
            print(name, flush=True)
            run('tc', 'qdisc', 'replace', 'dev', tx, 'root', 'pfifo' if internal else 'fq')
            for command in (['ethtool', '-K', tx], ['ip', 'netns', 'exec', namespace, 'ethtool', '-K', rx]):
                run(*command, 'tso', 'on' if offload else 'off', 'gso', 'on' if offload else 'off', 'gro', 'on' if offload else 'off')
            rule()
            listener = socket.socket()
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(('172.31.238.1', 54002)); listener.listen(); listener.setblocking(False)
            plan = {'seconds': 30, 'flows': flows, 'initial': 2 if name == 'old-new-groups' else flows, 'close': close, 'pause': pause}
            receiver = subprocess.Popen(['ip', 'netns', 'exec', namespace, sys.executable, str(Path(__file__).resolve()), '--receiver', json.dumps(plan)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            senders, params, snapshots = [], [], []
            start, changed, retired, sampled = time.monotonic(), set(), False, -1
            try:
                while receiver.poll() is None and time.monotonic() - start < 40:
                    elapsed = time.monotonic() - start
                    if change:
                        for second, rate in ((7, 10), (14, 2), (21, 5)):
                            if elapsed >= second and second not in changed:
                                rule(rate); changed.add(second)
                    if name == 'old-new-groups' and elapsed >= 8 and not retired:
                        ports.write_text('del 54002\n'); rule(); retired = True
                    ready, writable, _ = select.select([listener], senders, [], .02)
                    if listener in ready:
                        connection, _ = listener.accept()
                        connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_CONGESTION, b'brutal_review')
                        assert connection.getsockopt(socket.IPPROTO_TCP, socket.TCP_CONGESTION, 16).rstrip(b'\0') == b'brutal_review'
                        params.append(struct.unpack('=QIQ', connection.getsockopt(socket.IPPROTO_TCP, 23301, 20)))
                        connection.setblocking(False); senders.append(connection)
                    for connection in writable:
                        try:
                            connection.send(b'x' * 32768)
                        except BlockingIOError:
                            pass
                        except OSError:
                            senders.remove(connection); connection.close()
                    if int(elapsed) > sampled:
                        sampled = int(elapsed)
                        windows = []
                        for connection in senders:
                            info = connection.getsockopt(socket.IPPROTO_TCP, socket.TCP_INFO, 256)
                            # Linux UAPI TCP_INFO: tcpi_snd_wnd at offset 228.
                            windows.append(struct.unpack_from('=I', info, 228)[0] if len(info) >= 232 else None)
                        snapshots.append({'second': sampled, 'sender_windows': windows, 'groups': ports.read_text(), 'tcp': run('ss', '-n', '-t', '-i', '-O', 'sport', '=', ':54002').stdout})
                stdout, stderr = receiver.communicate(timeout=5)
                if receiver.returncode:
                    raise RuntimeError(stderr)
                received = json.loads(stdout)
                ticks = received['ticks']
                low, high = ticks[12 if retired else 5], ticks[-1]
                mbps = (high['bytes'] - low['bytes']) * 8 / (high['second'] - low['second']) / 1e6
                if not change:
                    target = 10 if retired else 5  # two independent groups coexist
                    assert abs(mbps / target - 1) <= .05, (name, mbps, target)
                if pause:
                    assert any(0 in x['sender_windows'] for x in snapshots if 8 <= x['second'] <= 11), 'zero receive window not observed'
                    assert int(ticks[14]['flows'].get('0', 0)) > int(ticks[11]['flows'].get('0', 0)), 'zero-window sender failed to recover'
                phases = []
                if change:
                    for begin, end, target in ((11, 13, 10), (17, 20, 2), (24, 28, 5)):
                        phase_mbps = (ticks[end]['bytes'] - ticks[begin]['bytes']) * 8 / (end - begin) / 1e6
                        phases.append({'from': begin, 'to': end, 'target_mbps': target, 'receiver_mbps': phase_mbps})
                        assert abs(phase_mbps / target - 1) <= .05, (name, phases)
                if retired:
                    assert len(set(p[2] for p in params)) == 2, 'old and new groups did not coexist'
                results.append({'name': name, 'receiver_mbps': mbps, 'phases': phases, 'socket_params': params, 'received': received, 'snapshots': snapshots, 'passed': True})
                output.joinpath('results.json').write_text(json.dumps(results, indent=2))
            finally:
                if receiver.poll() is None:
                    receiver.terminate(); receiver.wait(timeout=5)
                for connection in senders:
                    connection.close()
                listener.close()
                ports.write_text('del 54002\n')
        output.joinpath('exit-status.json').write_text(json.dumps({'exit_code': 0}))
    except BaseException as error:
        output.joinpath('exit-status.json').write_text(json.dumps({'exit_code': 1, 'error': str(error)}))
        raise
    finally:
        cleanup_errors = []
        try:
            if 'port=54002 active=1' in ports.read_text():
                ports.write_text('del 54002\n')
        except OSError as error:
            cleanup_errors.append(str(error))
        commands = []
        if owned_link:
            commands.append(('ip', 'link', 'del', tx))
        if owned_namespace:
            commands.append(('ip', 'netns', 'del', namespace))
        for args in commands:
            try:
                run(*args)
            except (OSError, subprocess.SubprocessError) as error:
                cleanup_errors.append(str(error))
        output.joinpath('cleanup-status.json').write_text(json.dumps({'cleaned': not cleanup_errors, 'errors': cleanup_errors}))
        if cleanup_errors:
            status_path = output / 'exit-status.json'
            status = json.loads(status_path.read_text()) if status_path.exists() else {}
            status.update(exit_code=1, cleanup_errors=cleanup_errors)
            status_path.write_text(json.dumps(status))
            raise RuntimeError('kernel test cleanup failed: ' + str(cleanup_errors))


if __name__ == '__main__':
    def terminate(signum, frame):
        raise KeyboardInterrupt('kernel regression terminated')
    signal.signal(signal.SIGTERM, terminate)
    if len(sys.argv) == 3 and sys.argv[1] == '--receiver':
        receive(json.loads(sys.argv[2]))
    else:
        main()
