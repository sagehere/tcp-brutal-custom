# Side-by-side Canary A/B

This branch packages the adaptive Phase 2 controller as a **separate Canary installation** that can coexist with the existing 2.1.9 baseline on the same host.

## Isolation

| Baseline 2.1.9 | Canary |
| --- | --- |
| kernel module `brutal` | `brutal_canary` |
| TCP CC `brutal` | `brutal_adaptive` |
| DKMS `tcp-brutal-custom` | `tcp-brutal-canary` |
| `/proc/net/tcp_brutal` | `/proc/net/tcp_brutal_canary` |
| `tbc2` | `tbc2-canary` |
| `/etc/tcp-brutal-custom` | `/etc/tcp-brutal-canary` |
| `/var/lib/tcp-brutal-custom` | `/var/lib/tcp-brutal-canary` |
| `/run/tcp-brutal-custom` | `/run/tcp-brutal-canary` |
| `tcp-brutal-custom-*` | `tcp-brutal-canary-*` |

The Canary eBPF selector also has independent map/program names and selects only `brutal_adaptive`.

## Stage 1 A/B model

Stage 1 intentionally uses **different local service ports**.

Example:

```text
443  -> baseline brutal
8443 -> Canary brutal_adaptive
```

The Canary manager refuses to add a port that is already active in the baseline `/proc/net/tcp_brutal/ports` table.

Same-port percentage routing such as 90% baseline / 10% Canary is **not** implemented in Stage 1. It is the planned Stage 2 experiment framework.

## Local Canary install

From this branch checkout:

```bash
sudo ./scripts/install-canary.sh
```

Default Canary panel port: `23334`.

Add a Canary test service port:

```bash
sudo tbc2-canary port add 8443 100
sudo tbc2-canary ports
```

Baseline commands remain unchanged:

```bash
sudo tbc2 port add 443 100
sudo tbc2 ports
```

## Remove Canary

```bash
sudo /usr/local/lib/tcp-brutal-canary/install-canary.sh --uninstall
```

To delete Canary config/history as well:

```bash
sudo /usr/local/lib/tcp-brutal-canary/install-canary.sh --uninstall --purge
```

The Canary uninstaller removes only Canary DKMS/modules, binaries, services and runtime state. It does not stop, replace or remove baseline `tcp-brutal-custom`.

## Update policy

Canary self-update is disabled. Canary versions are installed from an explicit branch checkout through `install-canary.sh`, so a test host cannot accidentally replace the production 2.1.9 release by pressing the normal update control.

## Validation on `kr`

Validated on Ubuntu 24.04.4 arm64 / Linux 6.17:

- `brutal` and `brutal_canary` loaded simultaneously;
- `brutal` and `brutal_adaptive` both appeared in `tcp_available_congestion_control`;
- both proc trees existed simultaneously;
- two SockOps selectors were attached simultaneously;
- baseline port 5260 produced accepted sockets using `brutal`;
- Canary port 5261 produced accepted sockets using `brutal_adaptive`;
- Canary rejected an attempt to add baseline-owned port 5260;
- Canary DKMS install/start/stop/uninstall completed while the baseline manager and port rule remained active;
- after Canary uninstall, a new baseline 5260 connection still used `brutal`.

## Next stage

Stage 2 can replace the separate-port experiment with one shared selector that assigns each accepted connection to baseline or Canary by a stable hash and configurable percentage, enabling controlled same-port 95/5 -> 90/10 -> 50/50 experiments.
