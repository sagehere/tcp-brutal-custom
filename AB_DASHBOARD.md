# A/B Dashboard (Step 4)

Step 4 surfaces the same-port A/B experiment and Stage 3 reporting contract in the Canary web panel.

The new **A/B Experiments** view can create and remove experiments, change the Canary percentage for new connections, show target versus actual allocation and selector failures, browse historical experiment ports and epochs, display readiness reasons, compare baseline and Canary metrics within one epoch, render cohort time-series, and download the existing portable A/B ZIP report for the selected time window.

“Ready for analysis” is a data-quality state only. It is not a performance verdict.

## Read-only APIs added

`GET /api/v1/ab/ports` returns distinct ports present in `ab_epochs`, including deleted experiments with retained history.

`GET /api/v1/ab/series?port=443&from=...&to=...&tier=minute` returns network cohort samples, selector samples, and optional application samples using the existing raw/minute/hour tiers. No storage schema is changed.

## Display calculations

Within the selected epoch, the browser displays retransmission rate, weighted mean RTT, goodput per member-second, application success rate, and application mean latency. Rows sharing a time bucket are aggregated by cohort before plotting; `gap=true` breaks a line segment.

0% and 100% epochs are shown as non-comparative rollout states and are not treated as network-ready two-cohort experiments.

All mutations reuse the existing authenticated A/B APIs and CSRF protection. The Stage 3 SQLite schema, retention rules, and ZIP report contract remain unchanged.