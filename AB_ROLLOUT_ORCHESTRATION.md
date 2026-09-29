# A/B Rollout Orchestration and Fixed Observation Windows (Step 6)

Step 6 adds a persisted rollout lifecycle on top of Step 5 statistical review.

## Principles

- Observation and evaluation may be automatic; traffic expansion is never automatic.
- Every stage attempt has a fixed start and fixed observation deadline.
- Before the deadline, the server never exposes an \`advance\` action.
- After the deadline, evaluation uses only the fixed window.
- Insufficient data does not silently extend a window; \`retry\` creates a new epoch and a new full window at the same percentage.
- Every lifecycle action is persisted in an audit log.

## Schema v3

Schema v3 adds:

- \`ab_rollouts\`: rollout plan, status, current stage and epoch;
- \`ab_rollout_stages\`: one row per fixed-window attempt;
- \`ab_rollout_events\`: create/pause/resume/retry/advance/rollback/complete/restart/stop audit events.

v1 and v2 databases migrate in place. Existing epochs, samples, and Step 5 \`ab_epoch_plans\` are preserved; legacy experiments are not given fabricated rollout histories.

## Rollout plan

A rollout plan contains strictly increasing Canary percentages and one observation-window duration.

The default Web plan is:

\`5% -> 10% -> 25% -> 50% -> 100%\`

with a 3600-second window per stage.

The initial Canary percentage must equal the first stage. A paused rollout is allowed to have a runtime configuration of 0%.

## Fixed-window states

Before the deadline, state is \`observing\`; only pause and rollback are available.

After the deadline the fixed-window Step 5 analysis determines the review state:

- \`eligible_review\`: advance/retry/pause/rollback;
- \`insufficient_review\`: retry/pause/rollback;
- \`guardrail_review\`: retry/pause/rollback;
- \`network_review_only\`: retry/pause/rollback;
- \`completion_review\`: complete/retry/rollback at the final stage.

The server, not the browser, determines \`allowed_actions\`.

## Pause and resume

Pause immediately routes new connections to 0% Canary, closes the current stage attempt, records an audit event, and leaves the rollout resumable.

Resume restores the current planned percentage but starts a new epoch and a new full fixed observation window. The remaining time from the pre-pause window is never reused.

## Retry

Retry closes the completed/insufficient attempt and starts the same percentage again in a new epoch with a fresh full window.

## Advance

Advance is accepted only when the server exposes it in \`allowed_actions\`.

It closes the current eligible attempt, applies the next percentage to new connections, creates a new epoch and stage attempt, and records the approval in the audit log.

No background code calls advance automatically.

## Rollback

Rollback routes new connections to 0% Canary and terminates the rollout with status \`rolled_back\`.

It is an end state, unlike pause.

## Complete

At the final stage, complete closes the rollout and records the operator action. It does not change the current Canary percentage.

## Restart semantics

A same-version manager restart that keeps the same epoch preserves the stage id and deadline.

If a code upgrade or recovery produces a new epoch, the old attempt is closed as \`restarted\`, a \`restart_window\` event is recorded, and the same stage starts a full new window. This prevents different code versions from sharing one inferential window.

Paused rollouts never auto-resume.

## Direct percentage-change protection

For experiments with a rollout plan, direct \`ab set\` / \`PUT /api/v1/ab/PORT\` percentage changes are rejected. Managed percentage changes must use rollout actions so the stage history and audit trail cannot be bypassed.

Unmanaged legacy A/B experiments retain manual percentage control.

## API

Read:

\`GET /api/v1/ab/rollout?port=443\`

Actions:

\`POST /api/v1/ab/rollout/443/pause\`

\`POST /api/v1/ab/rollout/443/resume\`

\`POST /api/v1/ab/rollout/443/retry\`

\`POST /api/v1/ab/rollout/443/advance\`

\`POST /api/v1/ab/rollout/443/rollback\`

\`POST /api/v1/ab/rollout/443/complete\`

## CLI

\`sudo tbc2-canary ab rollout 443\`

\`sudo tbc2-canary ab rollout 443 pause|resume|retry|advance|rollback|complete\`

Create with orchestration:

\`sudo tbc2-canary ab add 443 100 5 stages=5,10,25,50,100 window=3600\`

Use \`orchestrate=off\` for legacy manual percentage control.

## Dashboard

The A/B page now shows:

- current rollout and stage;
- fixed window deadline and remaining time;
- server-authorized actions;
- stage-attempt history;
- audit history.

The browser schedules one refresh just after a fixed deadline, but the server recalculates all permissions.

The Step 5 statistical card remains separate from the Step 6 lifecycle card.

## Export

Report ZIPs add \`rollout_history.json\`, containing matching rollout records, stage attempts, and full audit events. It is covered by \`checksums.sha256\`.

## Experiment deletion

Deleting a managed A/B experiment closes any active stage, marks the rollout \`stopped\`, records a stop event, and removes runtime configuration. Historical epochs, samples, rollout attempts, and audit events remain available for review and export.