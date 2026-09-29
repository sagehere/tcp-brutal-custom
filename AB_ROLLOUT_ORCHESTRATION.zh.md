# A/B 阶段编排与固定观察窗口（Step 6）

Step 6 在 Step 5 的统计分析之上增加 rollout orchestration：固定观察窗口、阶段尝试历史、暂停/恢复、回退、重试、人工批准下一档和完整审计日志。

Step 7 进一步增加独立的生产安全门：它可以阻止 `advance` / `complete` 并持久化生产告警，但不会自动修改流量。详见 `AB_PRODUCTION_SAFETY.zh.md`。

## 核心原则

- 系统可以自动观察和计算，但绝不自动扩流。
- 每个阶段尝试在开始时固定 \`started\` 和 \`observation_ends\`。
- 窗口到期前，后端不会提供 \`advance\`。
- 窗口到期后的统计只使用该固定窗口内的数据。
- 样本不足不能“悄悄延长”旧窗口；只能 \`retry\`，开启同一档新的完整窗口。
- 阶段推进必须由人工调用 \`advance\`。
- 所有阶段动作都写入持久化审计日志。

## Schema v3

A/B SQLite schema 从 v2 升级到 v3，新增三张表：

### \`ab_rollouts\`

保存一次完整 rollout：

- port
- created / ended
- status
- stages JSON
- observation window seconds
- current stage index
- current epoch id
- current stage-attempt id

### \`ab_rollout_stages\`

每一行代表一次固定窗口“尝试”，而不是仅代表一个比例：

- rollout id
- stage index
- Canary percent
- epoch id
- started
- observation ends
- ended
- end reason
- final state

因此同一个 10% 阶段可以存在多个 attempt，例如：

1. 第一次 10% 窗口样本不足；
2. 人工选择 retry；
3. 第二次 10% 从新 epoch 开始新的完整固定窗口。

### \`ab_rollout_events\`

保存完整审计事件：

- timestamp
- action
- from percent
- to percent
- epoch id
- detail

典型 action：

- create
- pause
- resume
- retry
- advance
- rollback
- complete
- restart_window
- stop

## 升级迁移

v1 或 v2 数据库打开时自动创建 v3 表并更新 schema metadata。

不会：

- 删除旧 epoch；
- 改写旧网络/应用样本；
- 改写 Step 5 的 \`ab_epoch_plans\`；
- 给旧实验伪造 rollout 历史。

因此已有 Step 5 历史继续保持原义。

## Rollout plan

实验创建时可以指定：

- stages，例如 \`5,10,25,50,100\`
- observation window seconds

默认 Web 编排：

\`5% -> 10% -> 25% -> 50% -> 100%\`

默认每档观察窗口：

\`3600 seconds\`

阶段必须严格递增，范围 1–100。

初始 Canary 比例必须等于第一阶段。

暂停状态的运行配置为 0%，因此重启时允许“当前配置 0% + 已暂停 rollout”的组合。

## 固定窗口

创建 rollout 时，第一阶段立即保存：

- stage id
- epoch id
- stage started
- observation ends

例如：

\`started = 10:00:00\`

\`observation_ends = 11:00:00\`

10:00–11:00 之间服务器状态固定为：

\`observing\`

允许：

- pause
- rollback

不允许：

- advance
- retry
- complete

即使 Step 5 的实时统计在 10:30 看起来已经满足 Guardrail，也不能提前推进。

11:00 后，服务器只使用 10:00–11:00 的数据做阶段复核。

## 窗口结束后的状态

### \`eligible_review\`

固定窗口已结束，Step 5 的预声明统计、样本量和 Guardrail 均满足。

允许人工：

- advance
- retry
- pause
- rollback

\`advance\` 才会真正改变新连接的 Canary 比例。

### \`insufficient_review\`

窗口结束但数据量或 Step 5 readiness 不足。

允许：

- retry
- pause
- rollback

不允许 advance。

retry 会：

1. 结束旧 stage attempt；
2. 保持同一个 Canary 档位；
3. 创建新的 epoch；
4. 从当前时间开始新的完整固定窗口。

### \`guardrail_review\`

窗口结束，至少一个预声明 Guardrail 区间跨界。

允许 retry / pause / rollback，不自动扩流。

### \`network_review_only\`

网络分析可复核，但没有足够应用层信息。

允许 retry / pause / rollback，不提供 advance。

### \`completion_review\`

最后一个阶段（通常 100%）的固定窗口结束。

允许：

- complete
- rollback
- retry

complete 只把 rollout 标记为完成，不会额外改变当前 Canary 比例。

## Pause

pause 是安全暂停，不是删除实验。

动作：

1. 新连接切换到 0% Canary；
2. 当前 stage attempt 立即关闭；
3. rollout 状态变为 paused；
4. 写入 audit event。

已有 TCP 连接自然结束，不主动断开。

暂停期间不会继续累计原固定窗口。

## Resume

resume：

1. 恢复当前阶段的预声明 Canary 百分比；
2. 创建新的 epoch；
3. 创建新的 stage attempt；
4. 从恢复时刻开始完整的新观察窗口。

不会继续使用暂停前剩余的窗口时间。

## Rollback

rollback：

1. 新连接切换到 0% Canary；
2. 当前 stage attempt 结束；
3. rollout 标记为 \`rolled_back\` 并结束；
4. 写入审计日志。

rollback 是本次 rollout 的终止动作，不等于 pause。

之后若要重新做 staged rollout，应删除/重新创建实验或开始新的实验计划。

## Advance

advance 只有服务器返回 allowed_actions 包含 \`advance\` 时才能执行。

执行后：

1. 关闭当前 stage attempt，记录 \`eligible_review / manual_advance\`；
2. 设置下一档 Canary 比例；
3. 创建新的 epoch；
4. 创建下一档完整观察窗口；
5. 写入 audit event。

系统不会自己调用 advance。

## Complete

最后一档窗口完成后可人工 complete。

complete：

- 结束 stage attempt；
- rollout status = completed；
- 保存审计事件；
- 保持当前 Canary 比例不变。

## Manager 重启与升级

### 同版本、同配置重启

如果 \`ensureABEpoch\` 继续使用原 epoch：

- stage id 不变；
- deadline 不变；
- 不重启观察窗口。

### 升级或恢复导致新 epoch

如果代码版本变化等原因产生新 epoch：

1. 旧 stage attempt 标记 \`restarted\`；
2. 写入 \`restart_window\` audit event；
3. 同一 stage index 创建新 attempt；
4. 新 epoch 从当前时刻获得完整新窗口。

这样不同代码版本不会混进同一个统计窗口。

### Paused rollout 重启

paused rollout 不自动恢复，不创建观察窗口。

仍需人工 resume。

## 防绕过

启用了 rollout plan 的实验不能再直接：

\`PUT /api/v1/ab/PORT {canary_percent: ...}\`

或：

\`tbc2-canary ab set PORT PERCENT\`

后端返回冲突，要求使用 rollout actions。

这样阶段历史、窗口和审计链不会被手动比例修改破坏。

未启用 rollout plan 的旧式 A/B 实验仍保留手动 \`ab set\` 兼容性。

## API

查询最近一次 rollout：

\`GET /api/v1/ab/rollout?port=443\`

返回：

- rollout plan / status
- current stage
- fixed deadline
- stage history
- audit events
- current fixed-window evaluation
- allowed_actions

动作：

\`POST /api/v1/ab/rollout/443/pause\`

\`POST /api/v1/ab/rollout/443/resume\`

\`POST /api/v1/ab/rollout/443/retry\`

\`POST /api/v1/ab/rollout/443/advance\`

\`POST /api/v1/ab/rollout/443/rollback\`

\`POST /api/v1/ab/rollout/443/complete\`

所有写操作继续受 session + CSRF 保护。

## CLI

查询：

\`sudo tbc2-canary ab rollout 443\`

动作：

\`sudo tbc2-canary ab rollout 443 pause\`

\`sudo tbc2-canary ab rollout 443 resume\`

\`sudo tbc2-canary ab rollout 443 retry\`

\`sudo tbc2-canary ab rollout 443 advance\`

\`sudo tbc2-canary ab rollout 443 rollback\`

\`sudo tbc2-canary ab rollout 443 complete\`

创建：

\`sudo tbc2-canary ab add 443 100 5 stages=5,10,25,50,100 window=3600\`

可关闭编排以保持传统手动模式：

\`orchestrate=off\`

## Dashboard

Step 6 在 A/B 页面新增“阶段编排与固定观察窗口”卡片：

- 当前 rollout 状态；
- 阶段计划；
- 当前 stage attempt；
- 固定窗口截止时间；
- 剩余时间；
- current epoch；
- allowed actions；
- stage attempt history；
- audit log。

浏览器在固定窗口到期后自动刷新一次，但动作权限始终由服务器重新计算。

统计卡与编排卡职责分离：

- Step 5 统计卡：解释数据和区间；
- Step 6 编排卡：决定当前允许的生命周期动作。

## 导出

A/B ZIP 新增：

\`rollout_history.json\`

包含时间范围内：

- rollout records；
- stage attempts；
- full audit events。

该文件同样进入 \`checksums.sha256\`。

## 删除实验

删除 A/B 实验时：

- 活动 rollout 的当前 stage 关闭；
- rollout 标记 stopped；
- 写入 stop audit event；
- runtime A/B 配置删除；
- epoch / samples / rollout / audit 历史继续保留并可导出。

## 解读纪律

- observing 不等于“结果未知所以可以先扩一点”。
- eligible_review 不等于“Canary 更好”。
- guardrail_review 不证明根因一定是拥塞算法。
- retry 不是延长旧窗口，而是新 epoch + 新固定窗口。
- pause 不是 rollback。
- complete 不等于修改流量。
- rollout actions 永远是人工动作。
