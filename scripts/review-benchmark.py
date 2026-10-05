#!/usr/bin/env python3
"""Isolated host-init_net TCP comparisons; never changes a physical interface.

Requires separately loaded brutal_base and brutal_review modules. Results are
receiver-side iperf3 JSON, ping RTT samples and live TCP snapshots, not a WAN test.
"""
import argparse
import json
import os
import re
import signal
import statistics
import subprocess
import sys
import threading
import time
from pathlib import Path


def command(*args, check=True, timeout=20):
    return subprocess.run(args, check=check, capture_output=True, text=True, timeout=timeout)


def percentile(values, fraction):
    return sorted(values)[max(0, int(len(values) * fraction + .999) - 1)] if values else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True)
    parser.add_argument('--repeats', type=int, default=5)
    parser.add_argument('--seconds', type=int, default=60)
    parser.add_argument('--warmup', type=int, default=10)
    parser.add_argument('--quick', action='store_true')
    parser.add_argument('--extended', action='store_true')
    parser.add_argument('--parallel', type=int, default=1)
    parser.add_argument('--cap', type=int, default=125, choices=range(100, 126))
    parser.add_argument('--offload', action='store_true')
    parser.add_argument('--matrix', action='store_true', help='all 18 bandwidth/RTT/loss combinations (about 7 hours)')
    parser.add_argument('--drop-mbps', type=int, help='reduce only the virtual bottleneck halfway through measurement')
    parser.add_argument('--mixed', action='store_true', help='two long flows and staggered 128-KiB responses')
    parser.add_argument('--continue-on-error', action='store_true', help='finish all attempts, record failures, and exit nonzero if any failed')
    parser.add_argument('--scenario', action='append', help='specific bandwidth_mbps,rtt_ms,loss_percent (repeatable)')
    parser.add_argument('--algorithms', default='cubic,bbr,brutal_base,brutal_review', help='comma-separated control subset')
    parser.add_argument('--target-percent', type=int, default=100, choices=range(1, 101), help='manual Brutal target as percent of simulated capacity')
    args = parser.parse_args()
    requested_algorithms = args.algorithms.split(',')
    if not requested_algorithms or len(set(requested_algorithms)) != len(requested_algorithms) or any(a not in ('cubic', 'bbr', 'brutal_base', 'brutal_review') for a in requested_algorithms):
        parser.error('invalid or duplicate congestion control subset')
    if os.geteuid() != 0:
        parser.error('root required')
    if args.seconds < 1 or args.repeats < 1 or args.parallel < 1:
        parser.error('positive duration, repeats and parallel required')
    if args.drop_mbps is not None and args.drop_mbps < 1:
        parser.error('positive reduced capacity required')
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=False)
    router, client = 'brv_router', 'brv_client'
    tx, rin, rout, rx = 'brv_tx', 'brv_in', 'brv_out', 'brv_rx'
    host, peer, port = '172.31.239.1', '172.31.239.6', '54001'
    owned_ns, owned_links, processes = [], [], []
    rules = ['/proc/net/tcp_brutal_base/ports', '/proc/net/tcp_brutal_review/ports']
    scenarios = [(20, 10, 0), (100, 80, 0), (100, 200, 1), (20, 80, 3)]
    if args.matrix:
        scenarios = [(bandwidth, rtt, loss) for bandwidth in (20, 100) for rtt in (10, 80, 200) for loss in (0, 1, 3)]
    if args.quick:
        scenarios = [(20, 10, 0)]
    if args.extended:
        scenarios = [(500, 80, 0)]
    if args.scenario:
        try:
            scenarios = [tuple(map(int, value.split(','))) for value in args.scenario]
            if any(len(s) != 3 or not 1 <= s[0] <= 100000 or not 0 <= s[1] <= 10000 or not 0 <= s[2] < 100 for s in scenarios):
                raise ValueError('scenario out of range')
        except ValueError as error:
            parser.error(str(error))
    existing_ns = {x.split()[0] for x in command('ip', 'netns', 'list').stdout.splitlines()}
    if router in existing_ns or client in existing_ns or command('ip', 'link', 'show', tx, check=False).returncode == 0:
        raise RuntimeError('test network identifiers already exist; refusing to reuse them')
    for path in rules:
        if not Path(path).exists():
            raise RuntimeError('load independent baseline/review modules first')
        if re.search(r'\bport=' + port + r' active=1\b', Path(path).read_text()):
            raise RuntimeError('test port already has a module rule')
    records, failures = [], []
    output.joinpath('results.json').write_text('[]')
    def terminate(signum, frame):
        raise KeyboardInterrupt('test run terminated')
    signal.signal(signal.SIGTERM, terminate)
    try:
        for namespace in (router, client):
            command('ip', 'netns', 'add', namespace)
            owned_ns.append(namespace)
            command('ip', '-n', namespace, 'link', 'set', 'lo', 'up')
        command('ip', 'link', 'add', tx, 'type', 'veth', 'peer', 'name', rin)
        owned_links.append(tx)
        command('ip', 'link', 'set', rin, 'netns', router)
        command('ip', '-n', router, 'link', 'add', rout, 'type', 'veth', 'peer', 'name', rx)
        command('ip', '-n', router, 'link', 'set', rx, 'netns', client)
        command('ip', 'addr', 'add', host + '/30', 'dev', tx)
        command('ip', 'link', 'set', tx, 'up')
        for namespace, device, address in ((router, rin, '172.31.239.2/30'), (router, rout, '172.31.239.5/30'), (client, rx, peer + '/30')):
            command('ip', '-n', namespace, 'addr', 'add', address, 'dev', device)
            command('ip', '-n', namespace, 'link', 'set', device, 'up')
        command('ip', 'route', 'add', '172.31.239.4/30', 'via', '172.31.239.2', 'dev', tx)
        command('ip', '-n', client, 'route', 'add', '172.31.239.0/30', 'via', '172.31.239.5')
        command('ip', 'netns', 'exec', router, 'sysctl', '-qw', 'net.ipv4.ip_forward=1')
        command('tc', 'qdisc', 'replace', 'dev', tx, 'root', 'fq')
        if not args.offload:
            command('ethtool', '-K', tx, 'tso', 'off', 'gso', 'off', 'gro', 'off')
            for namespace, device in ((router, rin), (router, rout), (client, rx)):
                command('ip', 'netns', 'exec', namespace, 'ethtool', '-K', device, 'tso', 'off', 'gso', 'off', 'gro', 'off')
        command('ip', 'netns', 'exec', client, 'ping', '-c', '1', '-W', '2', host)
        output.joinpath('environment.json').write_text(json.dumps({
            'kernel': command('uname', '-r').stdout.strip(), 'iperf': command('iperf3', '--version').stdout,
            'args': vars(args), 'available_algorithms': Path('/proc/sys/net/ipv4/tcp_available_congestion_control').read_text(),
            'host_qdiscs': command('tc', 'qdisc', 'show').stdout,
        }, indent=2))
        for bandwidth, rtt, loss in scenarios:
            for repetition in range(args.repeats):
                algorithms = requested_algorithms.copy()
                if repetition % 2:
                    algorithms.reverse()
                for algorithm in algorithms:
                    # Bound queueing to roughly 50 ms beyond propagation, and
                    # flush old packets before every comparison, not only scenarios.
                    queue_packets = max(64, int(bandwidth * 1e6 / 8 * (rtt / 2000 + .05) / 1400))
                    for device, extra in ((rin, []), (rout, ['loss', str(loss) + '%', 'rate', str(bandwidth) + 'mbit'])):
                        command('ip', 'netns', 'exec', router, 'tc', 'qdisc', 'replace', 'dev', device, 'root', 'netem', 'limit', str(queue_packets), 'delay', str(rtt / 2) + 'ms', *extra)
                    label = f'{bandwidth}mbps-{rtt}ms-{loss}loss-r{repetition}-{algorithm}'
                    print(label, flush=True)
                    rule = None
                    if algorithm.startswith('brutal_'):
                        rule = rules[algorithm == 'brutal_review']
                        cap = f' compensation_cap_percent={args.cap}' if algorithm == 'brutal_review' else ''
                        Path(rule).write_text(f'add {port} rate={bandwidth * 1000000 // 8 * args.target_percent // 100} gain=20{cap}\n')
                    server = None
                    if not args.mixed:
                        server = subprocess.Popen(['iperf3', '-s', '-1', '-B', host, '-p', port], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
                        processes.append(server)
                        time.sleep(.3)
                    ping = subprocess.Popen(['ip', 'netns', 'exec', client, 'ping', '-D', '-n', '-i', '.2', host], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                    processes.append(ping)
                    snapshots, stop = [], threading.Event()
                    def sample_tcp():
                        while not stop.wait(1):
                            try:
                                result = command('ss', '-H', '-n', '-t', '-i', '-O', 'sport', '=', ':' + port, timeout=3)
                                cpu = list(map(int, Path('/proc/stat').read_text().splitlines()[0].split()[1:]))
                                snapshots.append({'time': time.time(), 'tcp': result.stdout, 'host_cpu': cpu})
                            except (subprocess.SubprocessError, OSError) as error:
                                snapshots.append({'error': str(error)})
                    sampler = threading.Thread(target=sample_tcp)
                    sampler.start()
                    start = time.time()
                    group_stats = {}
                    drop_errors = []
                    def drop_capacity():
                        try:
                            command('ip', 'netns', 'exec', router, 'tc', 'qdisc', 'change', 'dev', rout, 'root', 'netem', 'limit', str(queue_packets), 'delay', str(rtt / 2) + 'ms', 'loss', str(loss) + '%', 'rate', str(args.drop_mbps) + 'mbit')
                        except (subprocess.SubprocessError, OSError) as error:
                            drop_errors.append(str(error))
                    drop_timer = threading.Timer(args.warmup + args.seconds / 2, drop_capacity) if args.drop_mbps else None
                    if drop_timer:
                        drop_timer.start()
                    trial_error = None
                    try:
                        if args.mixed:
                            fixture = subprocess.Popen([sys.executable, str(Path(__file__).with_name('review-transfer.py')), '--namespace', client, '--host', host, '--port', port, '--seconds', str(args.seconds), '--warmup', str(args.warmup), '--algorithm', algorithm], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                            processes.append(fixture)
                            try:
                                stdout, stderr = fixture.communicate(timeout=args.seconds + args.warmup + 60)
                            except subprocess.TimeoutExpired:
                                fixture.terminate(); fixture.communicate(timeout=5)
                                raise
                            if fixture.returncode:
                                raise RuntimeError(stderr)
                            result = subprocess.CompletedProcess([], fixture.returncode, stdout, stderr)
                        else:
                            result = command('ip', 'netns', 'exec', client, 'iperf3', '-c', host, '-p', port, '-R', '-C', algorithm, '-P', str(args.parallel), '-t', str(args.seconds), '-O', str(args.warmup), '-J', timeout=args.seconds + args.warmup + 60, check=False)
                        output.joinpath(label + '.stdout.txt').write_text(result.stdout)
                        output.joinpath(label + '.stderr.txt').write_text(result.stderr)
                        data = json.loads(result.stdout)
                        output.joinpath(label + '.json').write_text(json.dumps(data, indent=2))
                        if result.returncode or 'error' in data:
                            raise RuntimeError(data.get('error', f'client exit {result.returncode}'))
                    except BaseException as error:
                        detail = {'error': str(error)}
                        for name in ('stdout', 'stderr'):
                            value = getattr(error, name, None)
                            if value is not None:
                                detail[name] = value.decode(errors='replace') if isinstance(value, bytes) else value
                        output.joinpath(label + '.error.json').write_text(json.dumps(detail))
                        if not args.continue_on_error or isinstance(error, (KeyboardInterrupt, SystemExit)):
                            raise
                        trial_error = str(error)
                    finally:
                        if drop_timer:
                            drop_timer.cancel(); drop_timer.join()
                        stop.set(); sampler.join()
                        ping.terminate()
                        ping_output, _ = ping.communicate(timeout=5)
                        output.joinpath(label + '.tcp.json').write_text(json.dumps(snapshots, indent=2))
                        output.joinpath(label + '.ping.txt').write_text(ping_output)
                        if server:
                            if server.poll() is None:
                                server.terminate()
                            server.communicate(timeout=5)
                        # A finished userspace sender may leave a FIN-WAIT orphan.
                        # Restrict destruction to this run's synthetic endpoints.
                        cleanup = command('ss', '-K', '-t', 'src', host, 'sport', '=', ':' + port, 'dst', peer, check=False)
                        output.joinpath(label + '.socket-cleanup.txt').write_text(cleanup.stdout + cleanup.stderr)
                        if cleanup.returncode:
                            raise RuntimeError('test socket cleanup failed: ' + cleanup.stderr)
                        if rule:
                            for line in Path(rule).read_text().splitlines():
                                if line.startswith('port=' + port + ' active=1 '):
                                    group_stats = {k: int(v) for k, v in (field.split('=', 1) for field in line.split())}
                            Path(rule).write_text('del ' + port + '\n')
                    if trial_error is not None:
                        failures.append({'label': label, 'algorithm': algorithm, 'bandwidth_mbps': bandwidth,
                                         'rtt_ms': rtt, 'loss_percent': loss, 'error': trial_error,
                                         'module_stats_whole_session': group_stats})
                        output.joinpath('failures.json').write_text(json.dumps(failures, indent=2))
                        time.sleep(1)
                        continue
                    data['brutal_group_stats_whole_session'] = group_stats
                    data['capacity_drop'] = {'mbps': args.drop_mbps, 'measurement_second': args.seconds / 2, 'errors': drop_errors} if args.drop_mbps else None
                    output.joinpath(label + '.json').write_text(json.dumps(data, indent=2))
                    if not any(re.search(r'\b' + algorithm + r'\b', x.get('tcp', '')) for x in snapshots):
                        raise RuntimeError('actual sender algorithm was not verified: ' + algorithm)
                    rtts = [float(value) for timestamp, value in re.findall(r'\[([\d.]+)\].*time=([\d.]+)', ping_output) if float(timestamp) >= start + args.warmup]
                    end = data['end']
                    cpu_samples = [x for x in snapshots if 'host_cpu' in x and x['time'] >= start + args.warmup]
                    host_cpu = host_softirq = None
                    if len(cpu_samples) >= 2:
                        cpu_delta = [b - a for a, b in zip(cpu_samples[0]['host_cpu'], cpu_samples[-1]['host_cpu'])]
                        total_cpu = sum(cpu_delta[:8])
                        if total_cpu > 0:
                            host_cpu = 100 * (total_cpu - cpu_delta[3] - cpu_delta[4]) / total_cpu
                            host_softirq = 100 * cpu_delta[6] / total_cpu
                    records.append({'label': label, 'algorithm': algorithm, 'bandwidth_mbps': bandwidth, 'rtt_ms': rtt, 'loss_percent': loss,
                        'receiver_mbps': end['sum_received']['bits_per_second'] / 1e6,
                        'retransmits': end.get('sum_sent', {}).get('retransmits'), 'cpu': end.get('cpu_utilization_percent'),
                        'ping_p95_ms': percentile(rtts, .95), 'duration_seconds': time.time() - start,
                        'start_unix': start, 'queue_packets': queue_packets,
                        'drop_mbps': args.drop_mbps,
                        'target_percent': args.target_percent if algorithm.startswith('brutal_') else None,
                        'host_cpu_percent': host_cpu, 'host_softirq_percent': host_softirq,
                        'short_completion_ms': [x['completion_ms'] for x in data.get('mixed', {}).get('short_responses', [])],
                        'short_failures': data.get('mixed', {}).get('short_failures', []),
                        'module_sent_bytes_whole_session': group_stats.get('sent'),
                        'module_acked_bytes_whole_session': group_stats.get('acked'),
                        'module_retrans_bytes_whole_session': group_stats.get('retrans'),
                        'module_retrans_fraction_whole_session': group_stats.get('retrans', 0) / group_stats['sent'] if group_stats.get('sent') else None})
                    output.joinpath('results.json').write_text(json.dumps(records, indent=2))
                    if drop_errors:
                        raise RuntimeError('capacity drop failed: ' + str(drop_errors))
                    time.sleep(1)
        summary = []
        for bandwidth, rtt, loss in scenarios:
            for algorithm in requested_algorithms:
                selected = [x for x in records if (x['bandwidth_mbps'], x['rtt_ms'], x['loss_percent'], x['algorithm']) == (bandwidth, rtt, loss, algorithm)]
                summary.append({'bandwidth_mbps': bandwidth, 'rtt_ms': rtt, 'loss_percent': loss, 'algorithm': algorithm,
                    'runs': len(selected), 'median_receiver_mbps': statistics.median(x['receiver_mbps'] for x in selected) if selected else None,
                    'median_ping_p95_ms': statistics.median(x['ping_p95_ms'] for x in selected) if selected else None})
        output.joinpath('summary.json').write_text(json.dumps(summary, indent=2))
        if failures:
            raise RuntimeError(f'{len(failures)} failed trials; see failures.json (all requested attempts completed)')
        output.joinpath('exit-status.json').write_text(json.dumps({'exit_code': 0}))
    except BaseException as error:
        output.joinpath('exit-status.json').write_text(json.dumps({'exit_code': 1, 'error': str(error), 'valid_trials': len(records), 'failed_trials': len(failures)}))
        raise
    finally:
        cleanup_errors = []
        for process in processes:
            try:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill(); process.wait(timeout=5)
            except (OSError, subprocess.SubprocessError) as error:
                cleanup_errors.append(str(error))
        for path in rules:
            try:
                if Path(path).exists() and re.search(r'\bport=' + port + r' active=1', Path(path).read_text()):
                    Path(path).write_text('del ' + port + '\n')
            except OSError as error:
                cleanup_errors.append(str(error))
        for device in owned_links:
            try:
                command('ip', 'link', 'del', device)
            except (OSError, subprocess.SubprocessError) as error:
                cleanup_errors.append(str(error))
        for namespace in reversed(owned_ns):
            try:
                command('ip', 'netns', 'del', namespace)
            except (OSError, subprocess.SubprocessError) as error:
                cleanup_errors.append(str(error))
        output.joinpath('cleanup-status.json').write_text(json.dumps({'cleaned': not cleanup_errors, 'errors': cleanup_errors}))
        if cleanup_errors:
            status_path = output / 'exit-status.json'
            status = json.loads(status_path.read_text()) if status_path.exists() else {}
            status.update(exit_code=1, cleanup_errors=cleanup_errors)
            status_path.write_text(json.dumps(status))
            raise RuntimeError('test cleanup failed: ' + str(cleanup_errors))


if __name__ == '__main__':
    main()
