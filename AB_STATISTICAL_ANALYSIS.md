# A/B Statistical Analysis and Stage Review (Step 5)

Step 5 implements reproducible statistical calculations and predeclared rollout guardrails on top of the Stage 3 reporting contract and Step 4 Dashboard.

The system does **not** automatically increase Canary traffic. It only computes review states; any move to the next percentage still requires an explicit user action.

## Schema v2

A/B SQLite schema v2 adds `ab_epoch_plans`.

Each new epoch stores an immutable analysis plan containing:

- alpha and power;
- expected application success rate;
- application success non-inferiority margin;
- maximum retransmission delta;
- maximum mean RTT delta;
- minimum normalized goodput delta;
- bootstrap block length;
- a `predeclared` marker.

Existing v1 databases migrate automatically. Historical epochs that predate Step 5 do not receive fabricated plans; they remain viewable and exportable with `plan_predeclared=false`.

## Default technical template

Defaults for new experiments are:

- alpha: 0.05
- power: 0.80
- expected app success: 99.0%
- app success NI margin: 0.5 percentage points
- max retrans delta: +0.5 percentage points
- max mean RTT delta: +10%
- min normalized goodput delta: -10%
- bootstrap block: 5 minutes

These are implementation defaults, not universal business thresholds. The Dashboard lets the operator change them before the experiment begins.

## Application inference

Each cohort success rate uses a Wilson score interval.

The Canary-minus-baseline rate difference uses a Newcombe hybrid score interval.

Application non-inferiority passes only when:

`difference CI lower bound >= -NI margin`

A sample-size planning target is computed from the expected success rate, absolute margin, alpha, power, and configured allocation:

`N = (z_(1-alpha/2) + z_power)^2 * p*(1-p) * (1/w_baseline + 1/w_canary) / d^2`

The resulting total is split according to the configured baseline / Canary allocation.

## Network inference

Only the `minute` tier is used for inferential network calculations.

Baseline and Canary minute buckets are paired by timestamp inside one epoch. Buckets containing `gap=true` are excluded.

Metrics:

- retransmission difference in percentage points;
- mean RTT relative difference;
- normalized goodput relative difference.

Step 5 uses deterministic circular moving-block bootstrap:

- 2,000 replicates;
- default 5-minute blocks;
- a fixed per-epoch random seed;
- percentile confidence intervals using the epoch plan alpha.

At least `max(10, 2 * block_minutes)` valid paired minutes are required per metric.

Guardrail checks use conservative interval bounds:

- retrans CI upper <= predeclared max retrans delta;
- RTT CI upper <= predeclared max RTT delta;
- goodput CI lower >= predeclared minimum goodput delta.

## Review states

`collecting` means data quality, paired minutes, or application sample targets are still incomplete.

`guardrail_review` means at least one predeclared confidence interval crosses its allowed boundary and needs human review.

`network_review_only` means the network analysis is reviewable but application metrics are unavailable. This state does not offer a next-stage action.

`eligible_review` means predeclared plan, Stage 3 readiness, bootstrap availability, application sample target, and all guardrails are satisfied. It is not a claim that Canary is better.

`not_comparable` applies to 0% or 100% epochs where a simultaneous two-cohort comparison does not exist.

Standard manual rollout stages are:

`5% -> 10% -> 25% -> 50% -> 100%`

The Dashboard may show the next stage only for `eligible_review`, and the operator must explicitly confirm it.

## API and CLI

Read analysis:

`GET /api/v1/ab/analysis?port=PORT&from=UNIX&to=UNIX`

CLI:

`sudo tbc2-canary ab analysis PORT [FROM_UNIX TO_UNIX]`

CLI experiment creation can override the default plan:

`sudo tbc2-canary ab add 443 100 5 gain=20 success=99 ni=0.5 retrans=0.5 rtt=10 goodput=-10 block=5 alpha=0.05 power=0.8`

## Export

A/B report ZIPs now include:

`statistical_analysis.json`

This file uses the same epoch-level semantics as the API and Dashboard. Existing CSVs, analysis rules, and checksum verification remain intact.