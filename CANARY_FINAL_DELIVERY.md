# TCP Brutal Adaptive Canary — Final Delivery

This branch is the isolated Adaptive Canary delivery built beside the stable TCP Brutal Custom 2.1.9 baseline.

Final delivery branch:

```text
release/canary-v2.1.9-adaptive
```

It keeps the baseline and Canary resources independent: `brutal` / `brutal_canary`, `tbc2` / `tbc2-canary`, separate procfs, config, data, systemd services, DKMS package, eBPF selector, and Web port (23333 / 23334).

## One-command install or upgrade

Supported targets are Debian 12/13 and Ubuntu 22.04/24.04 on amd64/arm64 with systemd, cgroup v2, DKMS, and matching kernel headers.

```bash
curl -fsSL https://raw.githubusercontent.com/sagehere/tcp-brutal-custom/release/canary-v2.1.9-adaptive/scripts/bootstrap-canary.sh | sudo bash
```

The bootstrap performs compatibility checks, installs the isolated Canary stack, records exact build metadata, and verifies that the stable baseline CLI/config/services remain unchanged.

The Canary bootstrap builds from the GitHub branch directly. It does not use the signed stable-release chain and should not be confused with the production 2.1.9 release trust model.

## Quick verification

```bash
cat /etc/tcp-brutal-canary/build-info.json
systemctl is-active tcp-brutal-canary-manager.service
systemctl is-active tcp-brutal-canary-web.service
sudo tbc2-canary ab list
sudo tbc2 status
```

## Minimal staged A/B experiment

```bash
sudo tbc2-canary ab add 443 100 5 stages=5,10,25,50,100 window=3600
sudo tbc2-canary ab rollout 443
sudo tbc2-canary ab analysis 443
sudo tbc2-canary ab safety 443
```

Canary Web panel:

```text
http://SERVER_IP:23334
```

## Rollback

Managed rollout:

```bash
sudo tbc2-canary ab rollout 443 rollback
```

Manual experiment:

```bash
sudo tbc2-canary ab set 443 0
```

Percentage changes affect only newly established TCP connections.

## Export

```bash
sudo tbc2-canary ab export 443 /root/brutal-ab-443.zip
```

The report includes cohort/selector/application samples, epoch summaries, statistical analysis, rollout history, persistent safety alerts, analysis rules/plans, and SHA-256 checksums.

## Backup

```bash
sudo mkdir -p /root/tcp-brutal-canary-backup
sudo cp -a /etc/tcp-brutal-canary /root/tcp-brutal-canary-backup/
sudo cp -a /var/lib/tcp-brutal-canary /root/tcp-brutal-canary-backup/
```

## Uninstall

```bash
sudo /usr/local/lib/tcp-brutal-canary/install-canary.sh --uninstall
```

The uninstall is designed to remove only Canary resources and retain the stable 2.1.9 baseline.

## Final scope

The delivered stack includes:

- Phase 1 loss/RTT safeguards;
- Phase 2 per-socket adaptive pacing ceiling;
- isolated side-by-side Canary framework;
- stable same-port per-connection percentage routing;
- durable epoch-aware A/B reporting and portable ZIP export;
- A/B Dashboard;
- Wilson/Newcombe application inference and block-bootstrap network inference;
- predeclared statistical plans and sample targets;
- staged fixed-window rollout orchestration;
- pause/resume/retry/advance/complete/rollback with audit history;
- recent-window production safety guardrails;
- persistent active/cleared safety alerts;
- fail-closed rollout gating;
- explicit human-confirmed rollback.

External Webhook/Slack/Telegram/Email alert delivery is intentionally outside this final scope.

The main live-validation platform was Ubuntu 24.04.4 arm64 on Linux 6.17.0-1020-oracle with stable baseline 2.1.9. Validate again on each production target kernel, especially amd64 hosts.
