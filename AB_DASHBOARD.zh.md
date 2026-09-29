# A/B Dashboard（Step 4）

Step 4 把 Stage 2/3 的同端口分流、epoch、报表和 readiness 数据接入 Canary Web 面板。

## 页面能力

新增“**A/B 实验**”导航页：

- 新建同端口 A/B 实验；
- 查看当前目标比例、实际连接分配、selector failure；
- 动态切换 Canary 比例（0/5/10/25/50/100%）；
- 删除实验，保留历史 epoch 数据；
- 按 6 小时 / 24 小时 / 7 天 / 30 天 / 90 天查看；
- 浏览历史端口，即使实验已经删除；
- 选择 epoch，严格只在同一 epoch 内比较 baseline / Canary；
- 展示 network/application readiness 和未满足原因；
- 展示 retrans、RTT、归一有效吞吐、应用成功率、应用平均延迟；
- 展示按 raw/minute/hour 自动选择粒度的 cohort 趋势；
- 直接导出与当前端口/时间范围对应的 A/B ZIP 报表。

Dashboard 中“数据可分析”只表示达到 `AB_REPORTING_ANALYSIS.zh.md` 定义的数据完整性门槛，不代表 Canary 优于或劣于 baseline。

## 新增只读 API

### 历史实验端口

```text
GET /api/v1/ab/ports
```

返回 `ab_epochs` 中出现过的去重端口，因此删除实验后仍能从 Dashboard 访问历史数据。

### 时序数据

```text
GET /api/v1/ab/series?port=443&from=...&to=...&tier=minute
```

返回：

- `samples`：baseline / Canary 网络 cohort 样本；
- `selectors`：连接分配和 failure 样本；
- `app_samples`：可选应用层聚合样本。

`tier` 继续使用 Stage 3 的 `raw` / `minute` / `hour`，不增加新的存储 schema。

## 趋势计算

浏览器只对选中 epoch 的同 cohort 数据做显示：

- retrans = `retrans / sent`；
- mean RTT = `rtt_sum / rtt_samples`；
- normalized goodput = `acked * 8 / member_seconds`；
- application success = `success / requests`；
- application mean latency = `latency_sum / latency_samples`。

同一时间桶内多个 group/source 会先按 cohort 聚合。`gap=true` 会断开折线，避免把缺失区间画成连续趋势。

## Epoch 语义

Dashboard 不跨 epoch 合并结论。比例、rate、gain 或代码版本变化后，新的 epoch 独立显示。

0% / 100% epoch 会显示为“非双组对照”，可用于回退或全量验证，但不会被标成 network ready。

## UI 安全边界

- 比例修改仍调用既有 `PUT /api/v1/ab/PORT`，只影响新连接；
- 删除仍调用既有 `DELETE /api/v1/ab/PORT`，现有连接自然结束；
- Dashboard 不改变 Stage 3 SQLite schema、retention 或 ZIP 数据契约；
- 所有写 API 继续受 session + CSRF 保护。