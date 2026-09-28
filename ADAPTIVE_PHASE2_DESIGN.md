# Adaptive Brutal Phase 2

Date: 2026-09-28

Branch: `feature/adaptive-brutal-phase2`

Phase 2 builds on the phase-one congestion guardrails and adds a **per-socket adaptive pacing ceiling**. The configured Brutal rate remains the user's desired maximum and the shared group's scheduling budget; each socket may independently cap itself below that value when sustained delivery evidence shows that its path cannot support the configured target.

## Design goals

1. Preserve Brutal's configured-rate semantics instead of turning the algorithm into BBR.
2. Prevent a slow client from forcing the whole shared port group to a slow rate.
3. Use delivery evidence only when the sender is not application-limited.
4. Avoid reacting to one ACK or one short burst; use a longer sampling interval.
5. Recover upward automatically after a path improves.
6. Keep phase-one loss/RTT guardrails intact.

## Rate layers

Phase 2 deliberately separates three rates:

```text
configured group/socket rate
        |
        v
phase-one random-loss compensation
        |
        +----> shared-group virtual clock budget
        |
        v
per-socket adaptive ceiling
        |
        v
actual socket pacing rate + CWND sizing
```

The group clock is **not** reduced by a member's adaptive ceiling. This is what allows a fast member to use budget that a slow member cannot consume.

## Sampling

Sampling begins after a loss/congestion signal and only for non-application-limited traffic.

Current starting parameters:

- sampling interval: 500 ms;
- initial mismatch trigger: delivered rate below 80% of configured rate;
- low-utilization trigger after a ceiling exists: delivered rate below 80% of that ceiling;
- observed-loss correction: convert unique delivered rate back toward estimated wire-rate need;
- adaptive headroom: 120%;
- downward-ceiling confirmation: two consecutive sampling windows, using the higher safe estimate;
- upward probe: +25% every 1 second;
- probe hold: 500 ms.

The first mismatch is judged against the configured rate. Once a ceiling exists, the configured rate is no longer used as a reason to keep shrinking the ceiling. Further reductions require either clear phase-one RTT congestion or delivery below 80% of the current ceiling.

A single 500 ms window is not allowed to establish or lower a long-lived ceiling. Two consecutive downward samples are required, and the controller uses the higher safe estimate from those samples. This hysteresis was added after expanded matrix testing showed that one unlucky ACK/loss window could otherwise under-estimate a 100 Mbps path by roughly half.

This prevents the controller from repeatedly concluding that a healthy 100 Mbps path is "too slow" merely because the user configured 500 Mbps, while also avoiding one-sample overreaction.

## Shared-group safety

When multiple sockets share one Brutal group, plain delivery-rate mismatch is ambiguous because normal group sharing itself can make one member deliver far below the group's configured total.

Therefore:

- with one active group member, persistent loss plus a large configured/delivered mismatch may establish an adaptive ceiling;
- with multiple group members, conservative RTT-congestion evidence is required for mismatch-based downward adaptation;
- regardless of member count, each socket's ceiling remains private to that socket;
- `group->rate` is never lowered by the adaptive controller.

## Validation on `kr`

Test host:

- Ubuntu 24.04.4 LTS
- arm64
- Linux 6.17.0-1020-oracle
- veth + network namespace synthetic clients
- `netem` for rate, delay and random loss

### Single oversized connection

Configured target: 500 Mbps  
Synthetic path: 100 Mbps, 20 ms delay, 5% random loss

Final tuned Phase 2:

- 25 s goodput: **95.28 Mbps**
- adaptive socket pacing after convergence: approximately **106 Mbps**
- retransmitted bytes: 16.28 MB over 25 s
- maximum observed RTT: approximately 200 ms

For comparison, the earlier phase-one 15 s acceptance run under the same synthetic path delivered 94.31 Mbps, while the original main baseline delivered 92.40 Mbps and reached about 978 ms maximum RTT.

Phase 2's primary gain over phase one is not another large retransmission reduction; it is the automatic per-socket pacing ceiling that stops a 500 Mbps configuration from continuing to pace at 500 Mbps on a ~100 Mbps client path.

### Shared group: slow + fast client

Configured shared group target: 200 Mbps

Synthetic clients:

- slow path: 20 Mbps, 20 ms, 5% loss;
- fast path: 200 Mbps, 20 ms.

With both active for 20 seconds:

- slow client: **8.2 Mbps**
- fast client: **188.9 Mbps**
- aggregate: **197.1 Mbps**

The slow member did not reduce the fast member's available group budget.

After the fast client disconnected, a new slow-client-only connection on the same configured group reached:

- goodput: **19.0 Mbps**
- adaptive pacing: approximately **20.5 Mbps**

This demonstrates that the per-socket state is not persisted as a reduced group rate and a slow client can independently converge to its own path capacity.

## Known limits

This first Phase 2 implementation is intentionally conservative:

- adaptation requires loss/congestion evidence; a pure delay-only shaper with no loss may not establish a ceiling;
- when several long-lived members are active, the controller avoids treating ordinary group sharing as a path-capacity signal unless RTT congestion provides additional evidence;
- the current constants are starting points based on controlled synthetic A/B tests, not universal Internet constants;
- mobile networks, Wi-Fi, ACK compression, asymmetric paths and policers with large token buckets need additional validation.

The next engineering step after CI is broader matrix testing across path capacities, RTTs, loss rates, and connection counts before considering merge into main.


## Expanded matrix and dynamic validation

A broader synthetic matrix was run after the initial implementation. It covered 20, 50, 100 and 500 Mbps path capacities, 0-10% random loss, and 20/100 ms delay.

The first matrix exposed two controller issues:

1. a single bad 500 ms sample could establish an excessively low ceiling;
2. +10% probing every 2 seconds recovered too slowly after a path improved.

After adding two-window confirmation and accelerating non-congested probing, the previously problematic cases were re-tested:

| Path | Final goodput | Final/steady pacing |
| --- | ---: | ---: |
| 20 Mbps, 20 ms, 5% loss | 18.99 Mbps in confirmation run; 17.62 Mbps in later probe-tuning run | about 23-24 Mbps |
| 100 Mbps, 20 ms, 5% loss | 95.27 Mbps | about 118.6 Mbps |
| 100 Mbps, 20 ms, 10% loss | 95.00 Mbps | about 118.9 Mbps |
| 100 Mbps, 100 ms, 5% loss | 90.48 Mbps | about 103 Mbps after probing |
| 100 Mbps, 20 ms, 5% loss after final probe tuning | 95.16 Mbps | about 113.7 Mbps |

The 20 Mbps runs show some random-loss variance in short tests, but the controller no longer collapses into the ~8-11 Mbps range observed before confirmation hysteresis was added.

### Same-connection 100 -> 20 -> 100 Mbps transition

One TCP connection was kept open while the synthetic path capacity changed from 100 Mbps to 20 Mbps and then back to 100 Mbps, with 20 ms delay and 5% random loss throughout.

Observed behavior after final probe tuning:

- before the drop: roughly 93-102 Mbps delivered;
- after the drop: roughly 19-20 Mbps within a few seconds;
- pacing converged to roughly 24 Mbps;
- after capacity returned to 100 Mbps, delivered throughput recovered to about 67 Mbps after ~4-5 seconds, ~80 Mbps after ~5-6 seconds, and ~93 Mbps after ~6-7 seconds;
- subsequent one-second delivered throughput remained roughly 91-99 Mbps;
- pacing recovered from ~24 Mbps to ~114 Mbps in about 7 seconds.

This is substantially faster than the earlier +10% / 2 second probe policy, which mathematically required more than 30 seconds to climb from roughly 20 Mbps back toward 100 Mbps.

## Current merge-readiness assessment

The controller now behaves well across the targeted synthetic cases and dynamic capacity changes tested on `kr`. Remaining risk is concentrated in real-world path signatures that synthetic `netem` does not reproduce exactly, especially cellular/Wi-Fi variability, large-token-bucket policers, strong ACK compression and asymmetric congestion.

Before a production release, the preferred next step is a canary/real-network validation rather than further tuning solely against synthetic loss.
