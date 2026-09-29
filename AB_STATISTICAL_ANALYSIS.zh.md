# A/B 自动统计分析与阶段复核（Step 5）

Step 5 在 Stage 3 数据契约与 Step 4 Dashboard 上增加可重复的统计计算、预声明 Guardrail 与人工阶段复核。

核心原则：

- 统计计划必须在 epoch 开始前固化；
- 只比较同一 epoch 内同时发生的 baseline / Canary；
- 不把 10 秒样本当成独立观测；
- 不使用连续刷新 p-value 自动扩流；
- 系统只给出复核状态，不自动改变 Canary 比例；
- 点击“进入下一档”仍需要人工确认。

## 1. Schema v2

A/B SQLite schema 从 v1 迁移到 v2，新增：

`ab_epoch_plans`

每个新 epoch 保存一份不可变统计计划：

- alpha；
- power；
- expected application success rate；
- application success non-inferiority margin；
- maximum retransmission delta；
- maximum mean RTT delta；
- minimum normalized goodput delta；
- bootstrap block length；
- `predeclared` 标记。

已有 v1 数据库打开时自动创建该表并升级 schema metadata 到 v2。

旧 epoch 不会被伪造“预声明计划”。没有 `ab_epoch_plans` 记录的历史 epoch：

`plan_predeclared = false`

仍可浏览、导出和描述，但不会被标记为可以进入下一阶段复核。

## 2. 默认分析计划

CLI 或旧客户端未显式传入 plan 时，新实验使用以下默认技术模板：

| 参数 | 默认值 |
| --- | ---: |
| alpha | 0.05 |
| power | 0.80 |
| expected app success | 99.0% |
| app success NI margin | 0.5 percentage points |
| max retrans delta | +0.5 percentage points |
| max mean RTT delta | +10% |
| min normalized goodput delta | -10% |
| bootstrap block | 5 minutes |

这些值不是通用业务标准。

Dashboard 在创建实验时提供完整编辑项；应在看到实验结果前按业务容忍度修改。

## 3. 应用成功率

两组成功率分别使用 Wilson score interval。

Canary - baseline 的成功率差使用 Newcombe hybrid score interval。

Dashboard 显示：

- baseline success estimate + interval；
- Canary success estimate + interval；
- Canary - baseline difference；
- difference confidence interval；
- 预声明 non-inferiority margin。

非劣条件：

`difference CI lower bound >= -NI margin`

例如 NI margin = 0.5pp 时，95% difference CI 下界必须不低于 -0.5pp。

## 4. 应用样本量目标

Step 5 在实验开始前，根据以下参数计算应用成功率的粗略样本量目标：

- expected baseline success `p`；
- absolute margin `d`；
- alpha；
- power；
- 实际 Canary 分配比例。

固定分配比例下，总样本量近似：

`N = (z_(1-alpha/2) + z_power)^2 * p*(1-p) * (1/w_baseline + 1/w_canary) / d^2`

然后：

- baseline target = `ceil(N * w_baseline)`；
- Canary target = `ceil(N * w_canary)`。

因此 5% Canary 与 50% Canary 的样本量要求不会相同。

小 Canary 比例通常显著增加总实验流量需求，增长主要落在 majority cohort。

该公式是预规划近似，不替代正式业务统计设计。

## 5. 网络指标 block bootstrap

网络推断只使用 `minute` 层。

同一个 epoch 内，以时间戳配对 baseline / Canary minute bucket。

每个 minute 计算：

- retrans difference：Canary retrans% - baseline retrans%，单位 pp；
- RTT relative difference：` (Canary RTT - baseline RTT) / baseline RTT `；
- normalized goodput relative difference：` (Canary - baseline) / baseline `。

`gap=true` 的配对 bucket 不进入 bootstrap。

Step 5 使用 circular moving-block bootstrap：

- 默认 block = 5 minutes；
- 2,000 replicates；
- 每个 epoch 使用固定随机 seed，因此相同数据得到相同结果；
- percentile confidence interval 使用实验 plan 的 alpha。

为了避免极短序列产生虚假的窄区间，每个指标至少要求：

`max(10, 2 * block_minutes)`

个有效配对 minute。

## 6. 网络 Guardrail

系统使用 bootstrap interval 的保守边界进行阶段复核：

### Retransmission

要求：

`CI upper <= max_retrans_delta_pp`

### Mean RTT

要求：

`CI upper <= max_mean_rtt_delta_percent`

### Normalized goodput

要求：

`CI lower >= min_goodput_delta_percent`

这不是“Canary 胜出”的定义，只是在检查预声明容忍边界是否被置信区间穿越。

## 7. 阶段复核状态

### `collecting`

表示仍需采集。

常见原因：

- Stage 3 readiness 未满足；
- 成对 minute 不足；
- 应用样本量目标未满足；
- 历史 epoch 没有预声明 plan。

### `guardrail_review`

至少一项预声明 Guardrail 的置信区间未能保持在允许边界内。

这是人工复核信号，不等于自动判定根因来自拥塞算法。

### `network_review_only`

网络 readiness、bootstrap 和网络 Guardrail 可以复核，但没有应用层指标。

该状态不会提供自动进入下一档按钮。

### `eligible_review`

同时满足：

- 两 cohort 同时存在；
- plan 在 epoch 开始前固化；
- Stage 3 network readiness；
- network block bootstrap 有效；
- network Guardrail 未越界；
- application readiness；
- 预计算应用样本量目标；
- application non-inferiority interval 未越界。

Dashboard 此时可显示下一标准档位：

`5% -> 10% -> 25% -> 50% -> 100%`

但比例只有用户点击并确认后才修改。

### `not_comparable`

0% 或 100% epoch 没有同时存在的双 cohort 对照。

适用于回退或全量验证，不用于同 epoch A/B 推断。

## 8. API

新增：

`GET /api/v1/ab/analysis?port=PORT&from=UNIX&to=UNIX`

返回每个 epoch：

- 固化 plan；
- `plan_predeclared`；
- Wilson / Newcombe application intervals；
- sample target；
- block bootstrap network intervals；
- state；
- reasons；
- next stage percentage（如适用）。

## 9. CLI

查看自动统计分析：

`sudo tbc2-canary ab analysis PORT [FROM_UNIX TO_UNIX]`

创建实验时可以覆盖默认 plan：

`sudo tbc2-canary ab add 443 100 5 gain=20 success=99 ni=0.5 retrans=0.5 rtt=10 goodput=-10 block=5 alpha=0.05 power=0.8`

所有 plan 参数都会随实验配置保存；新 epoch 会把当时 plan 固化到数据库。

## 10. 导出

Stage 3 ZIP 增加：

`statistical_analysis.json`

其中包含与 Dashboard / `/api/v1/ab/analysis` 同一计算语义的 epoch 统计结果。

其余 Stage 3 文件继续保留，checksum 仍覆盖 ZIP 内所有分析文件。

## 11. 解读纪律

- `eligible_review` 不是“Canary 更好”。
- `guardrail_review` 不是“算法一定有问题”。
- 不跨 epoch 合并统计结论。
- 不把历史未预声明 epoch 伪装成 confirmatory experiment。
- 应用指标存在时，阶段推进要求应用层样本量与非劣条件。
- 没有应用指标时，只给 `network_review_only`。
- 阶段推进永远要求人工动作。