# A/B reporting, storage and analysis

This document defines the durable data contract for same-port Brutal baseline/Canary experiments.

## 1. Storage model

All Canary reporting data lives in the existing Canary SQLite database:

`/var/lib/tcp-brutal-canary/history.db`

SQLite remains in WAL mode and is included in the existing Canary backup/restore flow.

A/B data is isolated in dedicated tables:

- `ab_epochs`: immutable experiment phases. A new row is created when percentage, rate, gain or code version changes.
- `ab_samples`: network cohort counters for `baseline` and `canary`.
- `ab_selector_samples`: connection assignment counts and selector failures.
- `ab_app_samples`: optional application success/error/latency aggregates supplied by the service or a local sidecar.
- `ab_checkpoints` and `ab_selector_checkpoints`: restart-safe counter baselines.
- `ab_meta`: schema version.

A group id is not used to align cohorts because baseline and Canary have independent kernel groups. The join key is experiment port + epoch + cohort.

### Retention

The current default retention contract is:

| Tier | Resolution | Retention |
| --- | --- | --- |
| raw | 10 seconds | 30 days |
| minute | 1 minute | 365 days |
| hour | 1 hour | 3 years |

The normal history database size-pressure cleanup also reclaims A/B samples. Epoch metadata remains small and durable.

## 2. Epoch boundary rules

Percentage changes are analysis boundaries.

For example:

```text
5% -> epoch 17
10% -> epoch 18
25% -> epoch 19
```

Counters are checkpointed immediately at the boundary. Deltas from different percentages are never mixed.

Manager restart restores the open epoch only when percentage, rate, gain and code version are unchanged, then reseeds both kernel cohorts and selector counters before normal collection resumes.

## 3. Collection

Every 10 seconds the manager reads both kernel group trees:

```text
/proc/net/tcp_brutal/ports
/proc/net/tcp_brutal_canary/ports
```

For each A/B port it records, separately for baseline and Canary:

- bytes sent;
- bytes ACKed;
- retransmitted bytes;
- active members;
- RTT sum/sample count/max;
- sample duration and member-seconds.

It also records selector assignment counters:

- baseline connections;
- Canary connections;
- selector failures.

Application metrics are optional. A local root process can POST cohort-labelled aggregates to:

`POST /api/v1/ab/app-metrics`

The service should determine the cohort from the accepted socket's `TCP_CONGESTION` value and aggregate success/error/latency without storing client IPs.

## 4. Export contract

Portable export:

```bash
sudo tbc2-canary ab export PORT OUTPUT.zip [FROM_UNIX TO_UNIX]
```

The ZIP is a self-contained analysis package containing:

- `manifest.json`
- `epochs.csv`
- `cohort_samples.csv`
- `selector_samples.csv`
- `app_samples.csv`
- `summary.csv`
- `comparison.csv`
- `analysis_plan.json`
- `analysis_rules.json`
- `checksums.sha256`

Every exported payload is SHA-256 listed in `checksums.sha256`. The full SQLite database remains available through the normal backup mechanism; the report ZIP is intentionally a portable subset.

## 5. Analysis readiness

Readiness is not a winner/loser decision. It only answers whether an epoch has enough quality to be compared.

Current default network-readiness policy:

- at least 30 minutes;
- at least 200 assigned TCP connections in each cohort;
- selector failure rate <= 0.1%;
- connection randomization absolute z-score <= 4;
- no data-gap samples;
- percentage must be between 1% and 99%.

Application-readiness additionally requires at least 1,000 application requests in each cohort.

These are operational minimums, not statistical sample-size guarantees.

## 6. Comparison metrics

Compare only baseline and Canary from the same epoch.

Recommended metric priority:

1. application success/error rate, when instrumented;
2. application latency;
3. retransmission percent = retransmitted bytes / sent bytes;
4. RTT, weighted by RTT sample count;
5. effective throughput;
6. selector failure and assignment integrity.

Raw total throughput must not be compared directly when cohort sizes differ. `goodput_per_member_mbps` is provided as a normalized diagnostic, but still requires healthy randomization and enough concurrency.

## 7. Statistical analysis plan

### Application success

For each cohort report the success proportion and a Wilson confidence interval. Compare the absolute difference in success rates.

Before an experiment, define a service-specific non-inferiority margin. Do not choose the acceptable degradation after seeing the result.

For a binary outcome with baseline probability `p`, target detectable absolute difference `d`, two-sided alpha `alpha`, and power `1-beta`, a rough equal-size planning approximation is:

```text
n ~= 2 * (z_(1-alpha/2) + z_(1-beta))^2 * p * (1-p) / d^2
```

Use this for planning; use the actual randomized allocation ratio when computing final required cohort sizes.

### Network time series

Network samples are autocorrelated. Do not treat every 10-second row as an independent observation.

Use minute-tier paired baseline/Canary observations inside one epoch and estimate uncertainty with a time-block bootstrap. A 5-minute block is the current recommended starting point; sensitivity-check with longer blocks for high-RTT or highly bursty traffic.

Useful paired metrics include:

- retransmission-rate difference;
- weighted mean RTT difference;
- normalized goodput difference.

### Latency percentiles

Schema v1 stores application latency sum/count/max, so it supports mean and max only.

For production p50/p95/p99 analysis, schema v2 should add fixed application-side latency histogram buckets. Histograms should be cohort-labelled and aggregated before storage; raw request/client identifiers are unnecessary.

### Repeated rollout decisions

Do not continuously inspect ordinary p-values and promote as soon as one crosses a threshold.

For a staged rollout such as `5% -> 10% -> 25% -> 50%`:

- predefine a minimum duration and sample-size target for each stage;
- collect until both readiness and sample-size targets are met;
- evaluate the fixed stage window;
- expand, hold, or return to 0% based on predeclared application guardrails and network diagnostics.

## 8. Segmentation

The strongest comparison is the randomized aggregate because assignment is simultaneous on one service port.

If later segmentation is needed, add only coarse non-identifying dimensions such as:

- region;
- access-network class;
- RTT bucket;
- protocol/application class.

Do not export raw client IPs merely for analysis. Any segment field must be defined before the experiment and applied symmetrically to both cohorts.

## 9. Interpretation rules

- Never pool different epochs just to increase sample size.
- Never compare a Canary epoch to a baseline period from a different time when simultaneous baseline data exists.
- Treat selector failures, data gaps and severe allocation imbalance as validity problems.
- A short 5% stage with zero or a handful of Canary connections is descriptive only.
- Prefer application outcomes over transport-only improvements when the two disagree.
- Keep the exported ZIP and its checksum file with any published conclusion so the result can be reproduced later.
