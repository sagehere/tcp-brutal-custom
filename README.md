# TCP Brutal Custom

<p align="center">
  <img src="logo.png" width="400" alt="TCP Brutal">
</p>

<p align="center">
  Transparent TCP Brutal takeover by local server port, with shared-rate groups, a Web panel, history, DKMS deployment, and update/rollback tooling.
</p>

<p align="center">
  <a href="README.zh.md">中文文档</a> ·
  <a href="CUSTOM.zh.md">Detailed custom guide</a> ·
  <a href="https://github.com/HyNetworks/tcp-brutal">Upstream TCP Brutal</a>
</p>

> This project is based on [HyNetworks/tcp-brutal](https://github.com/HyNetworks/tcp-brutal) v2.0.1 and is distributed under GPL-3.0. The upstream project provides the Brutal congestion-control implementation and destination-rule support; this fork adds transparent takeover by **local TCP service port**, management and deployment tooling.

## What this fork adds

The main goal is simple: run a normal TCP service on a Debian/Ubuntu VPS and enable Brutal **without modifying the application**.

For example:

```bash
sudo tbc2 port add 443 100
```

New TCP connections accepted on local port `443` will use Brutal automatically. All connections on that port share a **100 Mbps target effective rate** as one group.

This is not 100 Mbps per connection:

- one active connection can use the whole group rate;
- multiple active connections dynamically share the same group rate;
- changing the rate updates existing managed connections immediately;
- adding or deleting a port rule affects only newly established connections.

IPv4 and IPv6 connections on the same local port use the same group.

## Features

- **Transparent port takeover** — select Brutal by local TCP server port; no application patching is required.
- **Shared-rate groups** — all managed connections on one port share one target rate instead of multiplying the rate per connection.
- **eBPF SockOps selector** — switches eligible passive TCP connections to the `brutal` congestion-control algorithm at connection establishment.
- **Web management panel** — manage ports, rates, CWND gain, access control, updates and history.
- **CLI management** — add/remove ports, inspect status, change panel settings, update or uninstall from SSH.
- **Traffic history** — sent, acknowledged and retransmitted bytes plus RTT statistics, stored in SQLite.
- **DKMS installation** — builds the kernel module for the running kernel and survives normal kernel upgrades.
- **Update/rollback flow** — stages a release, switches the module in a controlled maintenance flow, and attempts rollback if switching fails.
- **Upstream compatibility** — original destination-IP rules and socket parameter APIs remain available.

## Requirements

Supported installation targets:

- Debian 12 / 13
- Ubuntu 22.04 / 24.04
- amd64 / arm64
- systemd
- cgroup v2
- root access
- DKMS and matching kernel headers

The kernel module and eBPF selector interact with kernel internals. A successful build alone does not prove that takeover works on every kernel variant; validate on the actual target kernel before relying on it in production.

## One-command install

For normal installations:

```bash
curl -fsSL https://raw.githubusercontent.com/sagehere/tcp-brutal-custom/main/scripts/bootstrap.sh | sudo bash
```

The bootstrap does **not** blindly execute the Release installer. It embeds the project's Ed25519 release public key, verifies its fixed fingerprint, verifies the signature on `hashes.txt`, verifies the `install.sh` SHA-256 from that signed manifest, and only then executes the installer. The installer repeats signature and artifact verification.

To pin a specific signed release instead of `latest`:

```bash
curl -fsSL https://raw.githubusercontent.com/sagehere/tcp-brutal-custom/main/scripts/bootstrap.sh | \
  sudo env TCP_BRUTAL_RELEASE_TAG=v2.1.5 bash
```

> **First-install trust boundary:** the bootstrap itself comes from this GitHub repository. If your threat model includes a full compromise of the repository/account before first installation, independently verify the release-key fingerprint below before granting root privileges. Existing installations pin the key locally, so normal updates do not re-bootstrap trust from GitHub.

## Verified install

Releases use an offline Ed25519 signing key. The private key is not stored in GitHub or GitHub Actions.

Before a first install, obtain `keys/release-signing-pub.pem` and verify this SHA-256 fingerprint through a channel you trust independently of this GitHub repository:

```text
249a5abded1a497f8fe67f4cf5cd8e47d127b9cee2d9b1eac23042b7edfeceff
```

Then verify the release before running anything as root:

```bash
base=https://github.com/sagehere/tcp-brutal-custom/releases/latest/download
curl -fsSL "$base/hashes.txt" -o /tmp/hashes.txt
curl -fsSL "$base/hashes.txt.sig" -o /tmp/hashes.txt.sig
curl -fsSL "$base/install.sh" -o /tmp/tcp-brutal-custom-install.sh

openssl pkeyutl -verify -rawin -pubin \
  -inkey keys/release-signing-pub.pem \
  -sigfile /tmp/hashes.txt.sig \
  -in /tmp/hashes.txt

grep ' install.sh$' /tmp/hashes.txt >/tmp/install.check
(cd /tmp && sha256sum -c install.check)
sudo bash /tmp/tcp-brutal-custom-install.sh
```

The installer repeats signature verification for the signed manifest and SHA-256 verification for every downloaded artifact. After installation, the trusted public key is pinned locally and normal updates never replace it.

On first install, a random administrator password is printed. See [TRUST.md](TRUST.md) for the trust model and key fingerprint.

Default locations:

| Item | Path |
| --- | --- |
| Configuration | `/etc/tcp-brutal-custom/config.json` |
| History database | `/var/lib/tcp-brutal-custom/history.db` |
| Manager socket | `/run/tcp-brutal-custom/manager.sock` |
| Port rules | `/proc/net/tcp_brutal/ports` |
| Destination rules | `/proc/net/tcp_brutal/rules` |

## Web panel

The panel listens on:

```text
http://SERVER_IP:23333
```

After the first login, configure the allowed client IP list.

> The built-in panel uses HTTP. Passwords and sessions are therefore not encrypted in transit. If the panel is reachable over an untrusted network, place it behind an HTTPS reverse proxy and restrict access appropriately.

The panel provides:

- current managed ports, connection counts, and client IP addresses;
- target rate and CWND gain;
- expected unique bytes / actual transmitted bytes (including retransmissions), plus acknowledged bytes;
- retransmission ratio;
- mean and maximum RTT;
- historical charts and CSV/JSON export;
- panel settings, service autostart, and password management (minimum 8 Unicode characters);
- release/update status; maintenance upgrades themselves require local root CLI.

## CLI usage

Open the interactive manager:

```bash
sudo tbc2
```

Common commands:

```bash
# Overall status
sudo tbc2 status

# 443 shares a 100 Mbps target rate; default gain is 20 = 2.0x
sudo tbc2 port add 443 100

# 443 shares 80 Mbps, CWND gain 15 = 1.5x
sudo tbc2 port add 443 80 gain=15

# List configured ports
sudo tbc2 ports

# Remove the rule; existing connections keep running until they close
sudo tbc2 port del 443

# Change panel listener / allow-list
sudo tbc2 panel 0.0.0.0 23334 203.0.113.5

# Disable service autostart
sudo tbc2 autostart off

# Update from the latest release
sudo tbc2 update  # local root/SSH only

# Uninstall
sudo tbc2 uninstall
```

## How port takeover works

```text
Incoming TCP connection
        │
        ▼
cgroup v2 SockOps eBPF program
        │
        ├─ local port not configured ─────► normal TCP congestion control
        │
        └─ local port configured
                  │
                  ▼
      setsockopt(TCP_CONGESTION, "brutal")
                  │
                  ▼
          brutal kernel module
                  │
                  ▼
       local port → shared group
                  │
                  ▼
       Brutal pacing / loss compensation
```

The selector runs on the passive-established SockOps event. If the local port is enabled, it switches the socket to `brutal`. When Brutal initializes, the module looks up the local port and joins the socket to that port's shared group.

Port rules have priority over upstream destination-IP rules.

### Group behavior

The configured Mbps value is the group's **target effective delivered rate**, not a strict wire-rate shaper.

Brutal compensates for loss. When loss is observed, the actual sending rate can exceed the configured target in an attempt to maintain the delivered rate. Do not configure a target higher than the path can realistically sustain.

Bandwidth is scheduled dynamically across group members rather than divided into fixed per-connection shares.

### Rule lifecycle

- **Add a new port rule:** only future connections are taken over.
- **Change an existing port's rate or gain:** existing group members receive the new values immediately.
- **Delete a port rule:** new connections stop matching; existing managed connections continue until they close.

## Scope and limitations

TCP Brutal controls **locally generated outgoing TCP traffic** for a managed socket.

The custom local-port takeover currently does **not** cover:

- UDP;
- QUIC;
- pure routed/forwarded traffic;
- Docker bridge port mapping;
- sockets outside the host network namespace.

If your service is behind a container bridge or only forwards traffic without terminating TCP on the host, the host local-port selector will not see it as a normal host-side passive TCP connection.

Do not set Brutal as the system-wide default congestion-control algorithm. Unmatched sockets without explicit Brutal parameters can otherwise fall back to the module's low default rate.

## Upstream destination rules

The original TCP Brutal v2 destination-rule interface is still retained.

Example:

```bash
sudo brutalctl add 203.0.113.5/32 100
sudo brutalctl list
sudo brutalctl del 203.0.113.5/32
```

Destination rules group connections by remote prefix. For full details, see the [upstream TCP Brutal documentation](https://github.com/HyNetworks/tcp-brutal).

When a connection matches both mechanisms, the local **port rule is applied first**.

## Updates and maintenance

```bash
sudo tbc2 update
```

Authenticated Web sessions can check release information but cannot start an update. The disruptive maintenance operation is restricted to a local root caller on the manager Unix socket, for example through SSH.

Before any service is stopped, the updater verifies the offline Ed25519 signature on `hashes.txt` and then verifies every downloaded artifact against that signed manifest. Missing or invalid signatures fail closed.

After verification, the update flow:

1. stages the new version;
2. stops the management services;
3. disconnects TCP connections on managed ports;
4. unloads the current `brutal` module;
5. installs/switches the DKMS module and binaries;
6. loads the new module and restarts services.

If the module is still busy, the switch is aborted. If switching fails after changes begin, the installer attempts to restore the previous module and program.

Run updates during a maintenance window.

## Uninstall

Standard uninstall keeps configuration and history:

```bash
sudo tbc2 uninstall
```

To remove persistent configuration and history as well:

```bash
sudo /usr/local/lib/tcp-brutal-custom/install.sh --uninstall --purge
```

## Build from source

Kernel module / DKMS source package:

```bash
make
make dkms-tarball
```

Management program:

```bash
go build -o tbc2 .
```

Upstream-compatible CLI:

```bash
make -C tools
```

Useful checks:

```bash
make format-check
CGO_ENABLED=0 go test ./...
```

## Repository layout

| Path | Purpose |
| --- | --- |
| `brutal_cc.c` | Brutal congestion control, pacing and group scheduling |
| `brutal_ports.c` | Local TCP port → Brutal group rules |
| `brutal_rules.c` | Upstream destination-prefix rules |
| `brutal_sockopt.c` | Brutal socket options and application groups |
| `selector_linux.go` | cgroup SockOps eBPF port selector |
| `manager.go` | Root manager, API, port control and statistics |
| `main.go` | CLI, configuration and service modes |
| `history.go` | SQLite history, aggregation and retention |
| `web/` | Embedded management panel |
| `scripts/install.sh` | Install, update, rollback and uninstall flow |
| `tools/brutalctl.c` | Upstream-compatible `brutalctl` CLI |

## Relationship to upstream

This fork builds on [HyNetworks/tcp-brutal](https://github.com/HyNetworks/tcp-brutal), an implementation of Hysteria's Brutal congestion-control behavior for Linux TCP.

The custom additions in this repository focus on operational deployment:

- transparent takeover by **local service port**;
- shared per-port groups;
- eBPF-based socket selection;
- Web/CLI management;
- persistent history;
- DKMS installation and release-driven updates.

For a more detailed Chinese operational guide, see [CUSTOM.zh.md](CUSTOM.zh.md).

### Upstream synchronization

`.github/workflows/upstream-sync.yml` checks `HyNetworks/tcp-brutal:master` daily and can also be run manually.

When upstream has new commits, the workflow:

1. creates an `upstream-sync/<upstream-sha>` branch;
2. performs a normal Git merge, preserving upstream history;
3. pushes the branch only when the merge is conflict-free;
4. opens a PR against `main`;
5. lets the normal PR CI run.

It **never auto-merges and never publishes a release**. If the merge has conflicts, the job fails and prints the conflicting files instead of resolving them with an unsafe automatic strategy. Kernel/eBPF changes still require human review and target-kernel testing.

## Release signing

GitHub Actions first builds an **unsigned release bundle** and uploads it as a workflow artifact. The private signing key never enters GitHub. The bundle's `hashes.txt` is signed offline; a separate publication workflow accepts only that public signature, verifies the staged artifact byte-for-byte, and then creates the tag/Release.

A release must be signed outside GitHub with the offline Ed25519 private key:

```bash
bash scripts/sign-release.sh /path/to/release-signing-key.pem build
bash scripts/publish-release.sh vX.Y.Z /path/to/release-signing-key.pem build
```

Installers reject releases without a valid `hashes.txt.sig`. Existing systems pin the release public key locally; ordinary updates cannot silently replace the trust root. Key rotation therefore requires an explicit migration. The 2026-09-28 rotation from the lost original key to the new key is documented in TRUST.md; existing installations must run the explicit migration helper before updating.

See [TRUST.md](TRUST.md).

## License



GPL-3.0. See [LICENSE](LICENSE).

The upstream TCP Brutal code and this modified work remain subject to the GPL-3.0 license and the corresponding attribution requirements.
