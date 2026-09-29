# TCP Brutal Adaptive Canary 最终交付指南

本分支是基于正式 baseline **TCP Brutal Custom 2.1.9** 的隔离式 Adaptive Canary 交付版。

它不会替换正式 `brutal` / `tbc2` / baseline Web 面板，而是以独立资源并存：

| 正式 baseline | Adaptive Canary |
| --- | --- |
| `brutal` | `brutal_canary` |
| `brutal` TCP CC | `brutal_adaptive` TCP CC |
| `tbc2` | `tbc2-canary` |
| `/proc/net/tcp_brutal` | `/proc/net/tcp_brutal_canary` |
| `/etc/tcp-brutal-custom` | `/etc/tcp-brutal-canary` |
| `/var/lib/tcp-brutal-custom` | `/var/lib/tcp-brutal-canary` |
| Web 23333 | Web 23334 |

## 1. 最终交付分支

```text
release/canary-v2.1.9-adaptive
```

该分支用于本轮 Phase 1/2 + Canary Stage 1–7 的最终集成交付。

正式 `main` 的 2.1.9 安装路径保持独立。

## 2. 一键安装 / 升级

需要 root、systemd、cgroup v2、DKMS 和匹配的内核头。

支持：

- Debian 12 / 13
- Ubuntu 22.04 / 24.04
- amd64 / arm64

一键安装：

```bash
curl -fsSL https://raw.githubusercontent.com/sagehere/tcp-brutal-custom/release/canary-v2.1.9-adaptive/scripts/bootstrap-canary.sh | sudo bash
```

同一条命令也用于升级 Canary。

安装器会自动：

1. 检查 OS / 架构 / cgroup v2 / 内核头；
2. 拉取最终 Canary 分支；
3. 构建并安装 `brutal_canary` DKMS；
4. 安装 `tbc2-canary`、manager、Web 和独立 eBPF selector；
5. 写入 `/etc/tcp-brutal-canary/build-info.json`；
6. 检查 Canary module / TCP CC / procfs / manager / Web / BPF；
7. 对比安装前后的 baseline CLI、配置、systemd unit 和运行状态，验证没有误改正式 2.1.9。

> Canary bootstrap 直接从 GitHub 分支拉取源码构建，不走正式 2.1.9 的离线签名 Release 链。它适合作为测试/Canary 交付入口，不应与正式签名 release 的信任模型混为一谈。

## 3. 前置 baseline 要求

同端口 A/B 需要正式 baseline 提供：

```text
/proc/net/tcp_brutal/ports
```

如果已经加载 `brutal` 但缺少该接口，bootstrap 会拒绝继续，并要求先升级正式 baseline 到 2.1.9。

## 4. 安装后快速检查

```bash
cat /etc/tcp-brutal-canary/build-info.json
systemctl is-active tcp-brutal-canary-manager.service
systemctl is-active tcp-brutal-canary-web.service
lsmod | grep brutal
cat /proc/sys/net/ipv4/tcp_available_congestion_control
sudo tbc2-canary ab list
```

正常情况下可以同时看到：

```text
brutal
brutal_canary

brutal
brutal_adaptive
```

正式 baseline 应继续：

```bash
sudo tbc2 status
systemctl is-active tcp-brutal-custom-manager.service
systemctl is-active tcp-brutal-custom-web.service
```

## 5. 最小 A/B 示例

创建 5% Canary、逐步 5→10→25→50→100 的实验：

```bash
sudo tbc2-canary ab add 443 100 5 \
  stages=5,10,25,50,100 \
  window=3600
```

查看：

```bash
sudo tbc2-canary ab list
sudo tbc2-canary ab rollout 443
sudo tbc2-canary ab analysis 443
sudo tbc2-canary ab safety 443
```

Web：

```text
http://SERVER_IP:23334
```

## 6. 阶段模型

当前完整链路：

```text
Adaptive Brutal
→ 同端口稳定百分比分流
→ epoch / SQLite 持久化
→ Dashboard
→ 统计分析
→ 固定窗口 rollout
→ 生产安全护栏
→ 人工批准推进 / 暂停 / 回退
```

系统不会因为统计结果或 Critical 告警自动增加 Canary 比例。

Critical 可以：

- 持久化告警；
- hard-block `advance` / `complete`；
- 给出 `rollback_recommended`。

但真正 rollback 仍需要人工确认。

## 7. 快速回退

对于 rollout-managed 实验：

```bash
sudo tbc2-canary ab rollout 443 rollback
```

对于非编排实验，可以：

```bash
sudo tbc2-canary ab set 443 0
```

0% 只影响之后建立的新连接；已有 TCP 连接自然结束，不会中途切换拥塞控制算法。

## 8. Pause / Resume / Retry / Advance

```bash
sudo tbc2-canary ab rollout 443 pause
sudo tbc2-canary ab rollout 443 resume
sudo tbc2-canary ab rollout 443 retry
sudo tbc2-canary ab rollout 443 advance
sudo tbc2-canary ab rollout 443 complete
```

服务端会根据固定观察窗口、统计状态和生产安全护栏决定哪些动作出现在 `allowed_actions`。

不能通过直接 `ab set` 绕过受管 rollout。

## 9. 数据与导出

主要数据：

```text
/etc/tcp-brutal-canary/
/var/lib/tcp-brutal-canary/history.db
```

单端口实验导出：

```bash
sudo tbc2-canary ab export 443 /root/brutal-ab-443.zip
```

ZIP 包含：

- manifest
- epochs
- cohort / selector / application samples
- summaries / comparison
- statistical analysis
- rollout history
- safety alerts
- analysis plan / rules
- SHA-256 checksums

## 10. 升级前备份

建议重要实验升级前：

```bash
sudo mkdir -p /root/tcp-brutal-canary-backup
sudo cp -a /etc/tcp-brutal-canary /root/tcp-brutal-canary-backup/
sudo cp -a /var/lib/tcp-brutal-canary /root/tcp-brutal-canary-backup/
```

Canary schema 当前为 v4，升级逻辑保留旧 epoch / analysis plan / rollout / audit，不给历史 epoch 伪造新版本计划。

## 11. 完整卸载

```bash
sudo /usr/local/lib/tcp-brutal-canary/install-canary.sh --uninstall
```

卸载目标：

- 移除 `brutal_canary`
- 移除 Canary procfs / CLI / service / DKMS
- 不停止、不替换、不删除正式 baseline 2.1.9

重要历史数据如果需要长期保留，应先备份或导出。

## 12. 最终验收基线

本轮最终实机验收环境：

```text
Ubuntu 24.04.4 arm64
Linux 6.17.0-1020-oracle
baseline: tcp-brutal-custom 2.1.9
```

已覆盖：

- baseline / Canary 并存；
- 0/5/50/100% A/B 分流；
- existing connection 不切算法；
- manager 重启恢复；
- zero-share 0/100% 边界；
- epoch / report / checksum；
- schema v1→v2→v3→v4 迁移；
- Dashboard；
- Wilson / Newcombe；
- block bootstrap；
- 固定观察窗口；
- pause / resume / retry / advance / rollback；
- rollout audit；
- production safety hard block；
- persistent alert create/update/clear；
- Critical 不自动 rollback；
- Canary 卸载不影响 baseline。

## 13. 当前边界

- 实机完整验收主要在 arm64 / Ubuntu 24.04.4 上完成；amd64 走构建/兼容路径，但上线前仍应在目标机器验收。
- 当前应用延迟安全指标基于 mean latency；P95/P99 需要未来 histogram schema。
- 没有实现外部 Webhook / Slack / Telegram / Email 告警投递；这是本轮明确跳过的范围。
- 正式 baseline 与 Canary 应继续按独立生命周期维护。

相关设计文档：

- `ADAPTIVE_PHASE1_VALIDATION.md`
- `ADAPTIVE_PHASE2_DESIGN.md`
- `CANARY_SIDE_BY_SIDE.zh.md`
- `CANARY_SAME_PORT_AB.zh.md`
- `AB_REPORTING_ANALYSIS.zh.md`
- `AB_DASHBOARD.zh.md`
- `AB_STATISTICAL_ANALYSIS.zh.md`
- `AB_ROLLOUT_ORCHESTRATION.zh.md`
- `AB_PRODUCTION_SAFETY.zh.md`
