# Same-port Canary A/B routing

Stage 2 extends the side-by-side Canary framework with **same-port, per-connection percentage routing**.

## Command model

Create an A/B experiment:

```bash
sudo tbc2-canary ab add 443 100 5
```

Meaning:

- local service port: 443
- target rate: 100 Mbps
- Canary share: 5%
- baseline share: 95%

Change only the percentage for **new connections**:

```bash
sudo tbc2-canary ab set 443 10
sudo tbc2-canary ab set 443 25
sudo tbc2-canary ab set 443 50
sudo tbc2-canary ab set 443 0
```

List experiments and selector counters:

```bash
sudo tbc2-canary ab list
```

Remove the experiment:

```bash
sudo tbc2-canary ab del 443
```

## Selection semantics

The unified Canary SockOps selector owns each A/B port and uses:

```text
bpf_get_socket_cookie(socket) % 100
```

as a stable 0-99 bucket.

If the bucket is below `canary_percent`, the accepted socket gets:

```text
TCP_CONGESTION=brutal_adaptive
```

otherwise:

```text
TCP_CONGESTION=brutal
```

Changing the percentage affects only newly established connections. Existing sockets keep the congestion control selected when they were established.

## Kernel groups

For every A/B port, the Canary manager creates the same rate/gain group in both proc trees:

```text
/proc/net/tcp_brutal/ports
/proc/net/tcp_brutal_canary/ports
```

The selector then routes each accepted connection into the matching algorithm/group.

An A/B port must not remain configured in the baseline manager's persistent config. The Canary manager rejects such a port and asks the operator to remove it from baseline manager ownership first. This prevents two control planes from rewriting the same experiment port.

## Fast rollback

```bash
sudo tbc2-canary ab set 443 0
```

immediately makes every **new** connection use baseline `brutal`. Existing Canary connections drain naturally without being reset.

This makes 0% Canary the emergency rollback state.

## Validation on `kr`

Test port: 5270. Each percentage used 200 fresh loopback TCP connections and inspected `TCP_CONGESTION` on the accepted socket.

| Configured Canary % | Baseline | Canary |
| ---: | ---: | ---: |
| 0% | 200 | 0 |
| 5% | 190 | 10 |
| 50% | 100 | 100 |
| 100% | 0 | 200 |

The 5% test produced exactly 10/200 Canary sockets and the 50% test produced exactly 100/200, confirming deterministic percentage bucketing in this sample.

### Existing connection stability

A connection established while the experiment was at 0% reported `brutal`.

The percentage was then changed to 100% while that connection remained open:

```text
FIRST=brutal
FIRST_AFTER=brutal
SECOND=brutal_adaptive
```

So percentage changes do not mutate live connections.

### Persistence and delete

After `ab add 5270 100 5`, the Canary manager was restarted. `ab list` restored the 5% experiment from config.

After `ab del 5270`, both baseline and Canary kernel port groups became inactive and the experiment was removed from persistent Canary config.

## Recommended rollout

A conservative production sequence is:

```text
0% -> 5% -> 10% -> 25% -> 50%
```

Advance only after comparing goodput, retransmission, RTT, connection errors and application-level success rate between baseline and Canary cohorts.

The selector counters expose baseline / Canary / failure connection counts per experiment port.
