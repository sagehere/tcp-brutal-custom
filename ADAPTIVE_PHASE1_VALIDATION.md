# Adaptive Brutal Phase 1 validation

Date: 2026-09-28

Branch: `feature/adaptive-brutal-phase1`

Implementation commit under test: `d2f0b5f8972dc30cc17a507a29c83b6df7d2241c`

## Objective

Validate the phase-one congestion guardrails intended to reduce retransmission and queue growth when the configured Brutal target is higher than the receiving path can sustain, while checking that a normally sized target does not regress throughput.

Phase one intentionally does **not** implement automatic bandwidth estimation or a per-socket adaptive rate ceiling.

## Test host

- Host: dedicated test machine `kr`
- OS: Ubuntu 24.04.4 LTS
- Architecture: arm64
- Kernel: Linux 6.17.0-1020-oracle
- Kernel headers: present
- Test module: built from the branch above and loaded directly
- Baseline: `main` at `bf59ecb31301a7e1e88213999320585b0fd3c4db`

The branch module compiled and loaded successfully on this kernel. Repository CI also passed format checks, Go tests, DKMS package-layout checks and shell syntax checks.

## Topology

Synthetic host-local path:

```text
Brutal sender
    |
    v
veth-host
    |
    +-- netem: 100 Mbit/s, 20 ms delay, optional 5% random loss
    |
    v
isolated client network namespace
```

Each measurement ran for about 15 seconds. The server accepted a TCP connection and explicitly selected `TCP_CONGESTION=brutal`. Port rules supplied the configured shared-group rate.

## Results

### Oversized target: 500 Mbps configured, 100 Mbps path, 20 ms + 5% loss

| Metric | main baseline | Phase 1 | Change |
| --- | ---: | ---: | ---: |
| Goodput | 92.40 Mbps | 94.31 Mbps | +2.1% |
| Retransmitted bytes | 17.57 MB | 9.33 MB | -46.9% |
| Max observed RTT | 978 ms | 210 ms | -78.5% |

This is the primary phase-one acceptance case. The guardrails reduced retransmission substantially and prevented the extreme RTT inflation seen with the baseline while preserving, and slightly improving, delivered throughput.

### Matched target: 100 Mbps configured, 100 Mbps path, 20 ms + 5% loss

| Metric | main baseline | Phase 1 | Change |
| --- | ---: | ---: | ---: |
| Goodput | 94.98 Mbps | 95.29 Mbps | +0.3% |
| Retransmitted bytes | 9.58 MB | 9.32 MB | -2.6% |
| Max observed RTT | 97.7 ms | 41.2 ms | -57.8% |

No throughput regression was observed in the matched-rate case.

### Rate-only shaping sanity check

With a 500 Mbps configured target and a 100 Mbps rate-only synthetic bottleneck without active loss, Phase 1 delivered about 95.5 Mbps with zero TCP retransmitted bytes. Linux `netem rate` / TBF primarily scheduled the excess traffic in this setup, so this case was useful as a sanity check but did not reproduce the real-world retransmission failure mode by itself.

## Acceptance result

Phase one meets its stated objective under the tested synthetic conditions:

- materially lower retransmission under an oversized target plus congestion signals;
- materially lower RTT inflation;
- no observed throughput regression with a target matching the path capacity;
- successful build and module load on Linux 6.17 arm64.

## Limits of this validation

This is controlled synthetic validation, not proof for every production access network. Cellular, Wi-Fi, ISP policers, asymmetric paths, ACK compression and very long RTT paths can produce different loss/queue signatures.

The constants used by phase one are deliberately conservative starting points:

- maximum non-congested loss compensation: 110% of configured rate;
- congestion loss threshold: 2%;
- RTT inflation threshold: 125% of minimum observed SRTT.

They should be revisited only with additional A/B data across real target networks. Automatic discovery of a client's sustainable rate remains a separate phase-two feature.
