# A/B Production Safety Guardrails and Persistent Alerts (Step 7)

Step 7 adds an independent production-safety layer on top of Step 5 statistical review and Step 6 fixed-window rollout orchestration.

It is deliberately conservative: the system may detect problems, persist alerts, and block rollout progression automatically, but it does **not** automatically change traffic. A rollback remains an explicit human action.

## Safety vs statistical review

Step 5 asks whether a fixed experiment window satisfies a predeclared statistical review plan. Step 7 asks whether the most recent short production window already contains an operationally unacceptable regression.

Step 7 therefore uses separate thresholds, separate persistence, and raw 10-second samples.

## Schema v4

Schema v4 adds:

- `ab_epoch_safety_plans`: an immutable safety-plan snapshot for each new epoch.
- `ab_safety_alerts`: persistent active/cleared production alerts with first_seen, last_seen, cleared, severity, code, message, and detail JSON.

v1/v2/v3 databases migrate in place. Existing epochs, Step 5 analysis plans, rollouts, stage attempts, and rollout audit events are preserved. Historical v3 epochs are **not** given fabricated safety plans.

## Default technical template

New experiments use these defaults unless explicitly changed before the experiment starts:

- safety window: 300 seconds
- minimum assigned connections: 50
- minimum application requests per cohort: 100
- selector failure maximum: 1.0%
- allowed gap samples: 0
- retransmission delta maximum: +2.0 percentage points
- mean RTT delta maximum: +50%
- application error-rate delta maximum: +2.0 percentage points
- application mean-latency delta maximum: +50%

These are engineering defaults, not universal business SLAs.

## Evaluation

The manager evaluates safety every 10 seconds using the recent raw-tier window without crossing the current epoch boundary.

States:

- `clear`: enough recent traffic and no breach.
- `warming_up`: insufficient recent assigned connections. It is fail-closed for progression but does not create a breach alert.
- `warning`: a non-critical validity/safety issue such as excessive data gaps.
- `critical`: one or more critical thresholds are exceeded.
- `not_comparable`: 0% or 100% Canary, so a simultaneous two-cohort relative comparison does not exist.

Critical breach codes:

- `selector_failure`
- `retrans_regression`
- `rtt_regression`
- `app_error_regression`
- `app_latency_regression`

`data_gap` is a warning hard blocker because incomplete data should prevent progression without being treated as proof of algorithm harm.

Application thresholds are evaluated only after both cohorts meet the configured minimum application request count.

## Rollout gating

Step 6 rollout responses now include:

- `safety_status`
- `safety_blocked`
- `rollback_recommended`

A hard blocker removes `advance` and `complete`.

When a critical breach also recommends rollback, `retry` is removed as well, leaving only applicable safety actions such as pause/rollback.

If safety data are unavailable at an otherwise eligible/completion review point, progression is fail-closed.

No safety state invokes rollback automatically.

## Persistent alert lifecycle

On a new breach, an active alert is inserted.

If the same code remains active, last_seen and detail are updated rather than creating duplicates.

When the recent window recovers, the alert is marked inactive and cleared_ts is stored.

A warming-up window does not clear an existing active breach.

Alert collection continues when the Dashboard is closed.

## API and CLI

Current safety state:

`GET /api/v1/ab/safety?port=443`

CLI:

`sudo tbc2-canary ab safety 443`

Experiment creation can override the safety template:

`sudo tbc2-canary ab add 443 100 5 stages=5,10,25,50,100 window=3600 safe-window=300 safe-connections=50 safe-app=100 safe-selector=1 safe-gaps=0 safe-retrans=2 safe-rtt=50 safe-app-error=2 safe-app-latency=50`

## Dashboard

The A/B Dashboard includes a Production Safety card showing:

- recent safety state and hard-block state
- recent assigned connections
- selector failure rate
- gap count
- retransmission delta
- RTT delta
- application error-rate delta
- application latency delta
- active and cleared alerts
- an explicit manual rollback action when rollback is recommended

While an active experiment is selected, the browser refreshes only safety and rollout state every 15 seconds. The authoritative safety evaluation still runs server-side every 10 seconds.

## Export

A/B ZIP reports add `safety_alerts.json`, containing epoch safety-plan snapshots and alert history. It is covered by `checksums.sha256`.

## Interpretation discipline

- Critical does not prove the congestion-control algorithm is the root cause.
- Warming up is not an incident, but it blocks progression.
- Data gaps are validity failures, not performance conclusions.
- `eligible_review + safety_blocked` still cannot advance.
- A rollback recommendation is not an automatic action.
- 0%/100% epochs are not simultaneous comparative safety windows.
- Step 7 short-window hard thresholds do not replace Step 5 statistical inference.
