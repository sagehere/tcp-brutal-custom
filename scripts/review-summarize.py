#!/usr/bin/env python3
"""Summarize receiver throughput, observed dispersion and sending cost.

Read-only input. JSON output goes to stdout; no claim of WAN performance or
statistical significance is made from five runs. RTT is ICMP probe p95.
"""
import argparse
import json
import re
import statistics
from pathlib import Path


def stats(values):
    present = [value for value in values if value is not None]
    return {'available': bool(present), 'n': len(present), 'median': statistics.median(present), 'min': min(present), 'max': max(present)} if present else {'available': False, 'n': 0}


def jain(values):
    return sum(values) ** 2 / (len(values) * sum(value ** 2 for value in values)) if values and sum(values) else None


def interval_rate(raw, begin, end):
    intervals = [x['sum'] for x in raw.get('intervals', [])
                 if not x['sum'].get('omitted') and not x['sum'].get('sender')
                 and x['sum']['start'] >= begin and x['sum']['end'] <= end + .01]
    seconds = sum(x['seconds'] for x in intervals)
    return sum(x['bytes'] for x in intervals) * 8 / seconds / 1e6 if seconds else None


def summarize(directory):
    records = json.loads(directory.joinpath('results.json').read_text())
    failure_path = directory / 'failures.json'
    failures = json.loads(failure_path.read_text()) if failure_path.exists() else []
    warmup = json.loads(directory.joinpath('environment.json').read_text())['args']['warmup']
    keys = sorted({(r['bandwidth_mbps'], r['rtt_ms'], r['loss_percent'], r['algorithm']) for r in records + failures})
    output = []
    for bandwidth, rtt, loss, algorithm in keys:
        selected = [r for r in records if (r['bandwidth_mbps'], r['rtt_ms'], r['loss_percent'], r['algorithm']) == (bandwidth, rtt, loss, algorithm)]
        failed_runs = sum((r['bandwidth_mbps'], r['rtt_ms'], r['loss_percent'], r['algorithm']) == (bandwidth, rtt, loss, algorithm) for r in failures)
        fairness, sending_cost, short_p95, sender_cpu, receiver_cpu = [], [], [], [], []
        pre_drop, post_drop, post_drop_rtt = [], [], []
        for record in selected:
            raw = json.loads(directory.joinpath(record['label'] + '.json').read_text())
            streams = raw.get('end', {}).get('streams', [])
            rates = [x['receiver']['bits_per_second'] for x in streams if 'receiver' in x]
            if 'mixed' in raw:
                rates = [int(value) for value in raw['mixed']['measured_long_bytes'].values()]
            fairness.append(jain(rates))
            sent, acked = record.get('module_sent_bytes_whole_session'), record.get('module_acked_bytes_whole_session')
            sending_cost.append(sent / acked if sent is not None and acked else None)
            shorts = record.get('short_completion_ms', [])
            short_p95.append(sorted(shorts)[max(0, int(len(shorts) * .95 + .999) - 1)] if shorts else None)
            cpu = record.get('cpu') or {}
            sender_cpu.append(cpu.get('remote_total'))
            receiver_cpu.append(cpu.get('host_total'))
            drop = raw.get('capacity_drop')
            if drop:
                middle = drop['measurement_second']
                # Exclude the transition and five seconds on either side.
                pre_drop.append(interval_rate(raw, max(0, middle - 25), max(0, middle - 5)))
                post_drop.append(interval_rate(raw, middle + 5, raw['end']['sum_received']['seconds']))
                ping = directory.joinpath(record['label'] + '.ping.txt').read_text()
                rtts = sorted(float(value) for timestamp, value in re.findall(r'\[([\d.]+)\].*time=([\d.]+)', ping)
                              if float(timestamp) >= record['start_unix'] + warmup + middle + 5)
                post_drop_rtt.append(rtts[max(0, int(len(rtts) * .95 + .999) - 1)] if rtts else None)
        output.append({'bandwidth_mbps': bandwidth, 'rtt_ms': rtt, 'loss_percent': loss, 'algorithm': algorithm, 'runs': len(selected), 'indexed_failed_runs': failed_runs,
                       'receiver_mbps': stats(r['receiver_mbps'] for r in selected),
                       'icmp_load_p95_ms': stats(r['ping_p95_ms'] for r in selected),
                       'retransmits': stats(r['retransmits'] for r in selected),
                       'module_sent_per_acked_whole_session': stats(sending_cost),
                       'module_retrans_fraction_whole_session': stats(r.get('module_retrans_fraction_whole_session') for r in selected),
                       'sender_iperf_cpu_percent': stats(sender_cpu), 'receiver_iperf_cpu_percent': stats(receiver_cpu),
                       'host_cpu_percent': stats(r.get('host_cpu_percent') for r in selected),
                       'host_softirq_percent': stats(r.get('host_softirq_percent') for r in selected),
                       'same_path_jain_index': stats(fairness), 'short_response_trial_p95_ms': stats(short_p95),
                       'receiver_pre_drop_stable_mbps': stats(pre_drop), 'receiver_post_drop_stable_mbps': stats(post_drop),
                       'icmp_post_drop_p95_ms': stats(post_drop_rtt)})
        output[-1]['short_failed_requests'] = sum(len(r.get('short_failures', [])) for r in selected)
        output[-1]['short_completed_requests'] = sum(len(r.get('short_completion_ms', [])) for r in selected)
    indexed_labels = {r['label'] for r in failures}
    unindexed = sorted(p.name.removesuffix('.error.json') for p in directory.glob('*.error.json') if p.name.removesuffix('.error.json') not in indexed_labels)
    exit_path = directory / 'exit-status.json'
    return {'directory': str(directory), 'results': output, 'indexed_failures': failures,
            'unindexed_error_labels': unindexed, 'exit_status': json.loads(exit_path.read_text()) if exit_path.exists() else None,
            'notes': ['ICMP probe p95 is not a TCP RTT percentile.', 'Module cost includes warmup and does not count wire headers.', 'CPU is unavailable for the stdlib mixed fixture in the iperf CPU columns.', 'Five-run min/max describes observations, not a confidence interval.']}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directories', nargs='*', type=Path)
    parser.add_argument('--self-test', action='store_true')
    args = parser.parse_args()
    if args.self_test:
        assert stats([1, None, 3]) == {'available': True, 'n': 2, 'median': 2, 'min': 1, 'max': 3}
        assert stats([None]) == {'available': False, 'n': 0}
        assert jain([1, 1]) == 1 and jain([1, 0]) == .5 and jain([]) is None
        raw = {'intervals': [{'sum': {'start': 0, 'end': 1, 'seconds': 1, 'bytes': 125000, 'omitted': True}},
                             {'sum': {'start': 1, 'end': 2, 'seconds': 1, 'bytes': 125000, 'sender': False}}]}
        assert interval_rate(raw, 0, 2) == 1 and interval_rate(raw, 2, 3) is None
        import tempfile
        with tempfile.TemporaryDirectory() as temporary:
            p = Path(temporary)
            (p / 'results.json').write_text('[]')
            (p / 'environment.json').write_text('{"args":{"warmup":10}}')
            (p / 'failures.json').write_text(json.dumps([{'bandwidth_mbps': 20, 'rtt_ms': 10, 'loss_percent': 0, 'algorithm': 'cubic', 'label': 'failed'}]))
            (p / 'old.error.json').write_text('{}')
            summary = summarize(p)
            assert summary['results'][0]['runs'] == 0 and summary['results'][0]['indexed_failed_runs'] == 1
            assert summary['results'][0]['receiver_mbps']['available'] is False
            assert summary['unindexed_error_labels'] == ['old'] and summary['exit_status'] is None
        print('summary regression checks passed')
        raise SystemExit(0)
    if not args.directories:
        parser.error('at least one results directory required')
    print(json.dumps([summarize(directory) for directory in args.directories], indent=2))
