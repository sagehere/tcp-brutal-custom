#!/usr/bin/env python3
"""Two long flows plus staggered 128-KiB responses on one service port.

Used by review-benchmark.py --mixed. Application-level receive byte counts and
short-response completion times are returned; this is not an iperf3 measurement.
"""
import argparse
import errno
import json
import select
import signal
import socket
import struct
import subprocess
import sys
import time
from pathlib import Path


def receiver(args):
    start, next_short = time.monotonic(), args.warmup + 5
    sockets, connecting, bytes_by_flow = {}, {}, {0: 0, 1: 0}
    short_times, short_failures, measured, next_flow = [], [], {0: 0, 1: 0}, 2
    for index in (0, 1):
        connection = socket.create_connection((args.host, args.port), timeout=10)
        connection.sendall(struct.pack('!I', 0))
        connection.setblocking(False)
        sockets[connection] = (index, 0, time.monotonic())
    intervals, previous_bytes = [], 0
    try:
        while time.monotonic() - start < args.warmup + args.seconds:
            elapsed = time.monotonic() - start
            if elapsed >= next_short and elapsed < args.warmup + args.seconds - min(10, args.seconds / 3):
                initiated = time.monotonic()
                connection = socket.socket()
                connection.setblocking(False)
                index, next_flow = next_flow, next_flow + 1
                bytes_by_flow[index] = 0
                error = connection.connect_ex((args.host, args.port))
                if error not in (0, errno.EINPROGRESS, errno.EWOULDBLOCK):
                    short_failures.append({'flow': index, 'error': os_error(error), 'elapsed_ms': 0})
                    connection.close()
                else:
                    connecting[connection] = (index, 128 * 1024, initiated, struct.pack('!I', 128 * 1024))
                next_short += 5
            ready, writable, _ = select.select(list(sockets), list(connecting), [], .02)
            for connection in writable:
                index, limit, initiated, header = connecting[connection]
                error = connection.getsockopt(socket.SOL_SOCKET, socket.SO_ERROR)
                if error:
                    short_failures.append({'flow': index, 'error': os_error(error), 'elapsed_ms': (time.monotonic() - initiated) * 1000})
                    connecting.pop(connection); connection.close(); continue
                try:
                    header = header[connection.send(header):]
                except BlockingIOError:
                    continue
                except OSError as error:
                    short_failures.append({'flow': index, 'error': str(error), 'elapsed_ms': (time.monotonic() - initiated) * 1000})
                    connecting.pop(connection); connection.close(); continue
                if header:
                    connecting[connection] = (index, limit, initiated, header)
                else:
                    connecting.pop(connection)
                    sockets[connection] = (index, limit, initiated)
            for connection in ready:
                index, limit, initiated = sockets[connection]
                try:
                    data = connection.recv(65536)
                except BlockingIOError:
                    continue
                except OSError as error:
                    if not limit:
                        raise
                    short_failures.append({'flow': index, 'error': str(error), 'elapsed_ms': (time.monotonic() - initiated) * 1000})
                    sockets.pop(connection); connection.close(); continue
                if not data:
                    if limit and bytes_by_flow[index] != limit:
                        short_failures.append({'flow': index, 'error': 'incomplete response', 'elapsed_ms': (time.monotonic() - initiated) * 1000})
                    sockets.pop(connection); connection.close(); continue
                bytes_by_flow[index] += len(data)
                if elapsed >= args.warmup and not limit:
                    measured[index] += len(data)
                if limit and bytes_by_flow[index] >= limit:
                    short_times.append({'flow': index, 'bytes': bytes_by_flow[index], 'completion_ms': (time.monotonic() - initiated) * 1000})
                    sockets.pop(connection); connection.close()
            for pending in (connecting, sockets):
                for connection, info in list(pending.items()):
                    index, limit, initiated = info[:3]
                    if limit and time.monotonic() - initiated > 10:
                        short_failures.append({'flow': index, 'error': '10-second deadline', 'elapsed_ms': (time.monotonic() - initiated) * 1000})
                        pending.pop(connection); connection.close()
            if elapsed >= args.warmup and len(intervals) <= int(elapsed - args.warmup):
                total = sum(measured.values())
                intervals.append({'measurement_second': int(elapsed - args.warmup), 'long_bytes': total - previous_bytes})
                previous_bytes = total
        for pending in (connecting, sockets):
            for index, limit, initiated, *_ in pending.values():
                if limit:
                    short_failures.append({'flow': index, 'error': 'measurement ended before response', 'elapsed_ms': (time.monotonic() - initiated) * 1000})
        return {'measured_long_bytes': measured, 'all_bytes': bytes_by_flow, 'short_responses': short_times, 'short_failures': short_failures, 'intervals': intervals}
    finally:
        for connection in list(sockets) + list(connecting):
            connection.close()


def os_error(code):
    return errno.errorcode.get(code, str(code))


def sender(args):
    listener = socket.socket()
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind((args.host, args.port)); listener.listen(); listener.setblocking(False)
    command = ['ip', 'netns', 'exec', args.namespace, sys.executable, str(Path(__file__).resolve()), '--receiver', '--host', args.host, '--port', str(args.port), '--seconds', str(args.seconds), '--warmup', str(args.warmup)]
    client = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    sockets, headers, limits, sent, algorithms = [], {}, {}, {}, []
    start = time.monotonic()
    cpu_start = time.process_time()
    try:
        while client.poll() is None and time.monotonic() - start < args.warmup + args.seconds + 20:
            ready, writable, _ = select.select([listener, *headers], sockets, [], .02)
            if listener in ready:
                connection, _ = listener.accept()
                connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_CONGESTION, args.algorithm.encode())
                algorithms.append(connection.getsockopt(socket.IPPROTO_TCP, socket.TCP_CONGESTION, 16).rstrip(b'\0').decode())
                connection.setblocking(False); headers[connection] = b''
            for connection in ready:
                if connection == listener:
                    continue
                try:
                    data = connection.recv(4 - len(headers[connection]))
                except BlockingIOError:
                    continue
                if not data:
                    headers.pop(connection); connection.close(); continue
                headers[connection] += data
                if len(headers[connection]) == 4:
                    limits[connection], sent[connection] = struct.unpack('!I', headers.pop(connection))[0], 0
                    sockets.append(connection)
            for connection in writable:
                limit = limits[connection]
                amount = min(32768, limit - sent[connection]) if limit else 32768
                try:
                    sent[connection] += connection.send(b'x' * amount)
                    if limit and sent[connection] == limit:
                        sockets.remove(connection); connection.close()
                except BlockingIOError:
                    pass
                except OSError:
                    sockets.remove(connection); connection.close()
        stdout, stderr = client.communicate(timeout=5)
        if client.returncode:
            raise RuntimeError(stderr)
        data = json.loads(stdout)
        if any(algorithm != args.algorithm for algorithm in algorithms):
            raise RuntimeError('requested congestion algorithm not in effect')
        measured = sum(int(value) for value in data['measured_long_bytes'].values())
        return {'end': {'sum_received': {'bits_per_second': measured * 8 / args.seconds}},
                'mixed': data, 'sender_cpu_seconds': time.process_time() - cpu_start,
                'actual_algorithms': algorithms, 'tool': 'stdlib TCP application fixture'}
    finally:
        if client.poll() is None:
            client.terminate(); client.wait(timeout=5)
        for connection in sockets + list(headers):
            connection.close()
        listener.close()


if __name__ == '__main__':
    def terminate(signum, frame):
        raise KeyboardInterrupt('mixed fixture terminated')
    signal.signal(signal.SIGTERM, terminate)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--receiver', action='store_true')
    parser.add_argument('--host', required=True)
    parser.add_argument('--port', type=int, required=True)
    parser.add_argument('--seconds', type=int, required=True)
    parser.add_argument('--warmup', type=int, default=10)
    parser.add_argument('--algorithm')
    parser.add_argument('--namespace')
    options = parser.parse_args()
    print(json.dumps(receiver(options) if options.receiver else sender(options)))
