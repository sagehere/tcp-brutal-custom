# A/B 报表、存储与分析规范

本文定义同端口 Brutal baseline / Canary 实验的长期数据契约。

## 1. 数据存储

所有 Canary 报表数据继续存放在：

`/var/lib/tcp-brutal-canary/history.db`

仍使用 SQLite WAL，并自动进入现有 Canary 备份/恢复流程。

A/B 使用独立表：

- `ab_epochs`：实验阶段。比例、rate、gain 或代码版本变化就新建 epoch。
- `ab_samples`：baseline / canary 两个 cohort 的内核网络计数。
- `ab_selector_samples`：新连接分配数量与 selector failure。
- `ab_app_samples`：可选的应用层成功/错误/延迟聚合数据。
- `ab_checkpoints`、`ab_selector_checkpoints`：重启安全的累计计数基线。
- `ab_meta`：schema 版本。

baseline 与 Canary 的 kernel group id 本来就不同，因此不能靠 group id 对齐；分析主键是“实验端口 + epoch + cohort”。

### 保留周期

| 层级 | 粒度 | 保留 |
| --- | --- | --- |
| raw | 10 秒 | 30 天 |
| minute | 1 分钟 | 365 天 |
| hour | 1 小时 | 3 年 |

数据库达到容量压力时，现有 history 清理机制也会回收 A/B 样本；体积很小的 epoch 元数据保留。

## 2. Epoch 边界

任何分流比例变化都必须切新 epoch：

```text
5%  -> epoch 17
10% -> epoch 18
25% -> epoch 19
```

切换瞬间立即 checkpoint，两段累计计数不会串账。

manager 重启时，只有比例、rate、gain、代码版本全部不变才恢复原 epoch；随后先重新 seed 两套 group 和 selector 计数，再开始正常采样。

## 3. 采集内容

manager 每 10 秒同时读取：

```text
/proc/net/tcp_brutal/ports
/proc/net/tcp_brutal_canary/ports
```

每个 cohort 独立记录：

- sent bytes
- ACKed bytes
- retrans bytes
- active members
- RTT sum / samples / max
- sample duration
- member-seconds

selector 独立记录：

- baseline 新连接数
- Canary 新连接数
- selector failure

应用层指标是可选项。本机 root sidecar/业务程序可以提交：

`POST /api/v1/ab/app-metrics`

业务侧应从 accepted socket 的 `TCP_CONGESTION` 判断 cohort，再上报聚合后的成功率/错误率/延迟；不需要保存客户端 IP。

## 4. 导出机制

```bash
sudo tbc2-canary ab export PORT OUTPUT.zip [FROM_UNIX TO_UNIX]
```

ZIP 包含：

- `manifest.json`
- `epochs.csv`
- `cohort_samples.csv`
- `selector_samples.csv`
- `app_samples.csv`
- `summary.csv`
- `comparison.csv`
- `analysis_plan.json`
- `analysis_rules.json`
- `checksums.sha256`

所有文件都有 SHA-256 校验。完整 SQLite 仍由正常 backup 保存；ZIP 是便于离线分析、归档和共享的可移植子集。

## 5. 是否具备分析资格

readiness **不是胜负判断**，只回答“这段实验数据是否足够可靠，可以比较”。

默认 network readiness：

- 至少运行 30 分钟；
- baseline / Canary 各至少 200 条已分配 TCP；
- selector failure <= 0.1%；
- 实际随机分流相对目标比例的 |z| <= 4；
- 没有 gap 样本；
- Canary 比例必须在 1%–99%。

应用层 readiness 还要求两组各至少 1,000 个应用请求。

这些只是最低数据质量门槛，不等于统计样本量已经足够。

## 6. 指标优先级

只比较**同一个 epoch 内同时存在**的 baseline 与 Canary。

建议优先级：

1. 应用成功率/错误率；
2. 应用延迟；
3. retrans % = retrans bytes / sent bytes；
4. 按 RTT sample 数加权的 RTT；
5. 有效吞吐；
6. selector failure 与分流完整性。

样本量不同的两组不能直接比总吞吐。报表提供 `goodput_per_member_mbps` 作为归一化诊断，但仍要求随机分流正常且并发量足够。

## 7. 将来的统计分析方法

### 应用成功率

分别计算两组成功率及 Wilson 置信区间，并比较成功率的绝对差。

实验开始前必须预先定义业务可接受的非劣化边界，不能看完结果后再决定“允许下降多少”。

对于二项指标，baseline 成功率为 `p`、希望检测的绝对差为 `d`、双侧显著性 `alpha`、统计功效 `1-beta` 时，等样本粗略规划公式：

```text
n ~= 2 * (z_(1-alpha/2) + z_(1-beta))^2 * p * (1-p) / d^2
```

最终规划应按实际 Canary 分配比例修正两组样本量。

### 网络时序指标

10 秒样本存在明显时间自相关，不能把每行当独立样本。

使用 minute 层，在同 epoch 内构造同时段 baseline/Canary 配对差值，再进行**时间块 bootstrap**。默认从 5 分钟 block 开始，并对高 RTT、突发业务用更长 block 做敏感性检查。

适合的配对指标：

- retrans rate 差异；
- 加权 mean RTT 差异；
- normalized goodput 差异。

### 延迟 P50/P95/P99

当前 schema v1 的应用指标存 sum/count/max，因此只能可靠计算 mean/max。

未来 schema v2 应加入固定延迟直方图 buckets，再从聚合直方图计算 p50/p95/p99。仍然不需要保存单请求或客户端身份。

### 分阶段上线

不要不断刷新普通 p-value，一旦显著就扩流。

例如：

`5% -> 10% -> 25% -> 50%`

每一级都应：

- 预先规定最短运行时间；
- 预先计算样本量目标；
- 同时满足 readiness + 样本量才分析；
- 只分析固定阶段窗口；
- 根据预先定义的应用 guardrail 和网络诊断决定扩大、保持或回到 0%。

## 8. 分层分析

随机同端口总体对照始终是主分析。

将来确需分层时，只加入粗粒度、非身份化维度，例如：

- 地区；
- 接入网络类别；
- RTT 桶；
- 协议/业务类型。

不要为了分析导出原始客户端 IP。任何 segment 必须在实验前定义，并对两组完全对称。

## 9. 解读纪律

- 不跨 epoch 拼样本。
- 有同时 baseline 时，不拿 Canary 与另一个时间段的 baseline 比。
- selector failure、gap、严重分流偏差首先是实验有效性问题，不是算法性能。
- 5% 阶段如果只有零星 Canary 连接，只能描述，不能下结论。
- 应用指标与传输层指标冲突时，优先看应用结果。
- 对外形成任何结论时，同时保存对应 ZIP 和 checksum，保证日后可复现。
