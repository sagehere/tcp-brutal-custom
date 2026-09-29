# A/B 生产安全护栏与持久化告警（Step 7）

Step 7 在 Step 5 的统计复核和 Step 6 的固定窗口编排之外增加一层独立的生产安全机制。

它解决的是：

- 实验还没有走完统计观察窗口，但最近几分钟已经出现明显生产异常；
- Step 5 的长期统计状态可能仍是 collecting，但生产系统需要立即阻止继续扩流；
- 告警需要在浏览器关闭、manager 重启之后仍然保留；
- 异常恢复后需要留下 first seen / last seen / cleared 审计记录。

Step 7 的核心原则是：

**可以自动发现、自动阻断推进、自动记录告警，但默认不自动修改流量。**

即使出现 Critical，系统只会：

1. 把 rollout 的 advance / complete 从 allowed_actions 中移除；
2. 在严重退化时移除 retry；
3. 标记 rollback_recommended；
4. 在 Dashboard 提供“人工回退到 0%”入口；
5. 把告警持久化。

真正回退仍需要人工确认。

---

## 1. 与 Step 5 Guardrail 的区别

Step 5 回答：

> 固定观察窗口内，统计置信区间是否满足预声明实验条件？

Step 7 回答：

> 最近一个短安全窗口里，是否已经出现不能继续暴露生产流量的明显异常？

因此两者独立配置、独立保存。

### Step 5

- minute 配对；
- Wilson / Newcombe；
- block bootstrap；
- 样本量目标；
- 置信区间；
- 面向阶段复核。

### Step 7

- raw 10 秒采样；
- 最近窗口聚合；
- 明确硬阈值；
- persistent alerts；
- 面向生产安全。

一个实验只有 Step 5 eligible_review，但 Step 7 hard_block=false 时，才可能在 Step 6 中看到 advance。

---

## 2. Schema v4

A/B SQLite schema 从 v3 升级到 v4。

新增：

### ab_epoch_safety_plans

每个新 epoch 保存一份不可变 safety snapshot：

- window_seconds
- min_assigned_connections
- min_app_requests_per_cohort
- max_selector_failure_percent
- max_gap_samples
- max_retrans_delta_pp
- max_mean_rtt_delta_percent
- max_app_error_delta_pp
- max_app_latency_delta_percent
- predeclared

安全评估读取的是这个 epoch snapshot，而不是运行中的可变配置。

因此：

- rollout action 不会因为读取 live config 而和 manager mutex 相互锁死；
- 历史导出可以准确还原当时使用的阈值；
- 同一 epoch 的门槛不会在结果出来后被静默修改。

### ab_safety_alerts

持久化：

- port
- epoch id
- rollout id
- first_seen
- last_seen
- cleared
- severity
- code
- message
- detail JSON
- active

同一个 active code 再次触发时更新 last_seen 和 detail，不反复创建重复告警。

恢复正常后：

- active = false
- cleared_ts 写入
- 历史记录保留

---

## 3. v3 -> v4 升级

升级会：

- 创建安全计划表；
- 创建告警表；
- schema_version 更新为 4。

不会：

- 删除旧 epoch；
- 删除 Step 5 plan；
- 删除 rollout；
- 删除 rollout stage history；
- 删除 audit event；
- 给旧 v3 epoch 伪造 safety plan。

旧 v3 历史仍然保持原义。

实际运行升级到 Step 7 时，由于代码版本变化，活动 A/B 会按照既有恢复逻辑创建新的 epoch；这个新 epoch 才会保存 Step 7 safety snapshot。

---

## 4. 默认技术模板

新实验如果没有显式指定 safety plan，会使用：

| 参数 | 默认值 |
| --- | ---: |
| safety window | 300 秒 |
| minimum assigned connections | 50 |
| minimum app requests per cohort | 100 |
| selector failure maximum | 1.0% |
| allowed gap samples | 0 |
| retrans delta maximum | +2.0 pp |
| mean RTT delta maximum | +50% |
| app error-rate delta maximum | +2.0 pp |
| app latency delta maximum | +50% |

这些值是工程模板，不是通用 SLA。

应当在实验开始之前根据业务容忍度调整。

Dashboard 创建实验时提供全部字段。

---

## 5. 实时评估窗口

manager 每 10 秒采样一次。

Step 7 使用最近 safety_window 的 raw 层数据。

例如：

window = 300 秒

表示每次评估都看最近约 5 分钟。

窗口不会跨当前 epoch。

如果当前 epoch 只运行了 80 秒，则只使用这 80 秒。

---

## 6. warming_up

当最近安全窗口的已分配新连接少于：

min_assigned_connections

状态为：

warming_up

特点：

- hard_block = true
- rollback_recommended = false
- 不生成 persistent breach alert

原因是数据还不够，不应该把“样本不足”误报成生产故障。

但也不能在 safety 状态还未形成时继续推进。

已有 active 告警不会因为一次 warming_up 自动清除。

---

## 7. Selector failure

计算：

failure / (baseline + canary + failure)

如果超过：

max_selector_failure_percent

产生：

code = selector_failure
severity = critical
hard_block = true
rollback_recommended = true

这类问题属于分流执行层异常，不应被当成算法性能差异。

---

## 8. 数据 gap

最近 raw 安全窗口统计：

- baseline gap
- Canary gap
- selector gap

总数超过：

max_gap_samples

产生：

code = data_gap
severity = warning
hard_block = true
rollback_recommended = false

原因是数据完整性不足时不能继续扩流，但 gap 本身不证明 Canary 正在伤害业务。

warning 状态下可以人工 pause / rollback / retry，不能 advance / complete。

---

## 9. 重传安全阈值

最近窗口同时有 baseline / Canary 网络样本时：

baseline_retrans = baseline retrans / baseline sent

canary_retrans = canary retrans / canary sent

然后：

retrans_delta_pp = canary_retrans - baseline_retrans

如果：

retrans_delta_pp > max_retrans_delta_pp

产生：

retrans_regression
critical
rollback recommended

这和 Step 5 bootstrap CI 不同。

Step 7 看的是最近窗口的硬阈值，而不是长期统计区间。

---

## 10. RTT 安全阈值

两组都有 RTT sample 时：

mean_rtt_delta_percent =
(Canary RTT - baseline RTT) / baseline RTT

如果超过：

max_mean_rtt_delta_percent

产生：

rtt_regression
critical
rollback recommended

---

## 11. 应用错误率

只有 baseline / Canary 最近窗口都达到：

min_app_requests_per_cohort

才比较应用指标。

错误率：

errors / requests

计算：

app_error_delta_pp =
Canary error% - baseline error%

超过：

max_app_error_delta_pp

产生：

app_error_regression
critical
rollback recommended

如果应用埋点没有达到 minimum requests，应用指标不参与 Step 7 hard threshold。

网络安全检查仍然继续。

---

## 12. 应用平均延迟

应用层两组都达到最少请求量、并且存在 latency sample 时：

app_latency_delta_percent =
(Canary latency - baseline latency) / baseline latency

超过：

max_app_latency_delta_percent

产生：

app_latency_regression
critical
rollback recommended

当前 Step 7 沿用 Stage 3 应用聚合能力，只检查 mean latency。

P95/P99 仍需要未来 histogram schema 扩展。

---

## 13. 安全状态

### clear

最近窗口达到最少连接量，并且没有 breach。

hard_block = false

### warming_up

最近窗口连接量不足。

hard_block = true

不生成 breach alert。

### warning

存在非 Critical 安全问题，例如 data gap。

hard_block = true

通常不直接建议 rollback。

### critical

至少一个 Critical breach。

hard_block = true
rollback_recommended = true

### not_comparable

当前 Canary 比例为：

0% 或 100%

没有同时存在的 baseline / Canary 双组，因此不进行相对安全比较。

该状态本身不是故障。

---

## 14. Rollout 权限联动

Step 6 rollout view 新增：

- safety_status
- safety_blocked
- rollback_recommended

当 safety_blocked=true：

- advance 被移除；
- complete 被移除。

如果 rollback_recommended=true：

- retry 也被移除；
- 只保留当前状态下适用的 pause / rollback 等安全动作。

因此即使 Step 5 是 eligible_review，Critical safety 仍会阻止扩流。

如果 safety 数据不可用，而 rollout 已经进入 eligible_review / completion_review：

系统 fail-closed：

- safety_status = unavailable
- safety_blocked = true
- advance / complete 不出现

---

## 15. 不自动 rollback

Step 7 默认不会自动调用：

rollback

这是有意设计。

原因包括：

- 应用异常可能来自上游服务而不是拥塞控制；
- 网络异常可能来自路径切换；
- 极端场景需要现场保留证据；
- 自动回退本身也是流量控制动作。

因此 Critical 会提供：

rollback_recommended = true

Dashboard 显示：

“人工回退到 0%”

只有用户确认后才执行。

对于 rollout-managed experiment：

调用 rollout rollback。

对于传统手动 A/B：

设置 Canary percentage = 0。

已有 TCP 连接继续自然结束，不主动断开。

---

## 16. 持久化告警生命周期

后端 collector 每 10 秒重新评估。

新 breach：

创建 active alert。

同一 code 持续：

更新 last_seen 和 detail。

恢复正常：

写 cleared_ts，active=false。

浏览器关闭不会影响告警采集。

manager 重启后，告警历史仍在 SQLite。

Dashboard 显示最近 24 小时：

- active
- cleared

完整历史保存在导出包。

---

## 17. API

查询当前生产安全状态：

GET /api/v1/ab/safety?port=443

返回：

- evaluation
- active alerts
- recent 24h alerts

evaluation 包含：

- safety plan
- recent metrics
- status
- hard_block
- rollback_recommended
- breaches
- reasons

---

## 18. CLI

查看：

sudo tbc2-canary ab safety 443

创建实验时可以覆盖安全模板：

sudo tbc2-canary ab add 443 100 5 \
  stages=5,10,25,50,100 \
  window=3600 \
  safe-window=300 \
  safe-connections=50 \
  safe-app=100 \
  safe-selector=1 \
  safe-gaps=0 \
  safe-retrans=2 \
  safe-rtt=50 \
  safe-app-error=2 \
  safe-app-latency=50

---

## 19. Dashboard

Step 7 新增“生产安全护栏”卡片。

显示：

- safety status
- hard block
- recent window
- recent assigned connections
- selector failure
- gap count
- retrans delta
- RTT delta
- application error delta
- application latency delta
- breach reasons
- persistent alerts
- cleared timestamps
- manual rollback button

## 20. 服务端与浏览器刷新

安全告警的计算不依赖 Dashboard。

manager collector 每 10 秒：

- 写入 raw A/B 样本；
- 重新计算最近 safety window；
- 新建/更新/清除 persistent alert。

活动 A/B 页面另外每 15 秒轻量刷新：

- /api/v1/ab/safety
- /api/v1/ab/rollout

浏览器关闭、网络断开或无人查看面板都不会停止服务端安全评估。

---

## 21. 导出

A/B ZIP 增加：

safety_alerts.json

其中保存：

- 时间范围内的 epoch safety plan snapshot；
- active alert；
- cleared alert；
- first_seen / last_seen / cleared；
- severity / code / message / detail。

该文件与其他报表文件一样进入 checksums.sha256。

---

## 22. 解读纪律

- Critical 不等于“已经证明 Canary 算法有 bug”。
- warning 不等于“可以忽略”。
- data gap 是数据有效性问题，不是性能结论。
- warming_up 不允许推进，但不是生产故障。
- eligible_review + safety_blocked 仍然不能推进。
- rollback recommendation 不是自动动作。
- 0% / 100% 不做双组相对安全比较。
- Step 7 的短窗口硬阈值不能替代 Step 5 的统计推断。
- Safety plan 必须在实验开始前按具体业务 SLA / SLO 调整，默认值只是工程模板。