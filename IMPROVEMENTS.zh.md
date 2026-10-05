# 稳健优化：接口、兼容性和复验

基线 `bf59ecb`；默认 gain 和最高 25% 补偿不变。未添加自动调速、客户端公平算法或锁结构重写，未替换正式服务。实现与测试交付阶段未发布 Release；后续按用户授权单独进行 v2.1.10 代码推送与签名发布。

## 受管端口与出口预算

`compensation_cap_percent` 为整数 100～125，100 关闭补偿；新增端口省略时默认为 125。CLI、Web、配置、备份恢复、端口 API 和内核规则均支持。旧 API/CLI/内核规则更新省略该字段时保留已有上限；配置文件升级时省略则恢复原来的 125。显式 0 和越界值被拒绝。12/20 字节应用 socket ABI 不变，目标地址规则和应用组仍采用原来的默认补偿。

```bash
sudo tbc2 budget set '[{"name":"wan","capacity_mbps":100,"reserve_percent":10}]'
sudo tbc2 port add 443 70 gain=20 compensation_cap_percent=110 budget=wan
sudo tbc2 port add 8443 10 compensation_cap_percent=100 budget=wan
sudo tbc2 budgets
sudo tbc2 diagnose
```

预算容量单位 Mbps，范围 0.5～1000000，预留比例范围 `[0,100)`，省略时为 10%。名称使用 1～48 位字母、数字、`_` 或 `-`，端口引用须存在。预算默认关闭，由用户提供容量，不自动推断。API `GET/PUT /api/v1/budgets` 读取或整体替换预算数组；已有端口仍引用的预算不能被删除。

预计负载 = 所属启用端口的 `目标 Mbps × 补偿上限 / 100` 之和；可用容量 = `容量 × (1－预留比例/100)`。返回 `requested_mbps`、`available_mbps`、`excess_mbps`、`warning` 及 `reduce_targets_percent`：按原补偿上限等比例下调合计目标的建议幅度，或者增加实测容量。它不自动应用，也不能保证极小预算下每个目标仍高于 0.5 Mbps 的配置下限。预算不是硬限速，且不包含协议头/隧道开销、未归属端口、旧连接组、目标地址组和其他流量；界面和接口明确提示这些未覆盖负载。不要据此认为出口已被安全隔离。

端口 `POST /api/v1/ports` 示例：

```json
{"port":443,"rate_mbps":70,"gain":20,"enabled":true,"compensation_cap_percent":110,"budget":"wan"}
```

省略 `budget` 更新时保留原归属，显式空字符串（CLI `budget=`）取消归属。认证及 Web CSRF 检查沿用现有实现。诊断接口 `GET /api/v1/diagnose` 只读，复用 `ss`、`tc`、`sysctl`；每个命令最多 3 秒、64 KiB，缺工具或错误返回 `available:false`，缺失 TCP 字段不是零。不修改 sysctl、拥塞算法或物理 qdisc。

## 自检、统计与调度

`tbc2 probe-bpf` 分别创建 IPv4/IPv6 临时监听端口、临时内核规则和独立 BPF 对象，检查实际算法、目标速率、gain、组 ID 及双向收发；每个网络操作有截止时间。失败返回非零并标明阶段，退出时关闭对象、连接和临时规则。安装器执行完整自检，失败触发回滚且不记录安装完成。环回自检不作为提速证据。

SQLite `user_version=1` 事务迁移增加采样起止时间、有效时长和口径版本，保留旧累计计数。首次、重启后的首区间、计数回退、时钟异常和大于 15 秒的间隔标为缺口；速率返回 `null`，不补零、不连线。旧记录 `legacy_timing:true`，保留原计数但不伪造时间精度。正常区间按时间重叠分配到分钟/小时，整数分配保持字节总量；共时覆盖一致的多组汇总相加后端速率，不把组时长合并为一个分母。若同桶各组的起止/时长不一致（例如旧组中途退出），该桶标记 `partial_coverage:true`，保留字节但不给可信速率；目前不推断未记录的组退出时间，也不把缺失尾部补零。

API 保留 `expected_bytes` / `actual_bytes` 等旧字段，新增 `start_ms`、`end_ms`、`duration_ms`、`timing_version`、`timing_valid`、`legacy_timing`、`partial_coverage`、`acked_mbps`、`sent_mbps`。CSV 在原有列后追加时间、速率和覆盖信息；没有速率时为空。主图展示 ACK 吞吐与含重传发送吞吐，ACK 吞吐不是接收端应用 goodput。RTT 仅在有效 RTT 测量事件更新，均值为采样 SRTT，最大值仍为组生命周期累计最大值；不能据 sum/count/max 推导分位数。数据库清理使用独立串行维护任务；阻塞超过采样容忍值仍反映为缺口。

共享预留保存原始计费速率，修改参数或离组前按最终实际发送量结算。时钟退款/补账有界，避免无符号下溢和异常未来等待；最深保留 2 ms 积分，最远预留 1 秒。复用原 CA 私有字段而非扩大状态；原内核私有大小检查保留。该上界不是公平性保证，海量并发等待的行为仍需进一步测量。

## 可重复验证

本机功能回归（Linux）：

```bash
CGO_ENABLED=0 go test ./...
cc -O2 -Wall -Wextra -Werror tools/clock-test.c -o /tmp/brutal-clock-test
/tmp/brutal-clock-test
node --check web/app.js
node tools/web-metrics-test.cjs
make format-check
bash -n scripts/*.sh
```

专用 Linux 测试机上，从**隔离源码目录**构建验证模块：

```bash
# 若从 Windows 生成的源码压缩包解包，恢复 DKMS 辅助脚本执行权限
chmod u+x scripts/mkdkmsconf.sh
make BRUTAL_MODULE=brutal_review
sudo insmod brutal_review.ko
sudo python3 scripts/review-kernel-regression.py --output /tmp/brutal-kernel-new
```

验证模块注册 `brutal_review`，使用 `/proc/net/tcp_brutal_review`，不覆盖 `brutal`。管理程序需交叉编译时通过 Go `-ldflags '-X main.algorithmName=brutal_review -X main.portsPath=/proc/net/tcp_brutal_review/ports ...'` 同时指定独立 `configDir`、`dataDir`、`socketPath` 和 `panelPasswordFile`。该构建拒绝系统安装/更新/恢复/开机启动操作。不要只改算法名、仍使用正式路径。

性能对照另外需要把基线源码中的算法 `brutal`、proc 名 `tcp_brutal` 和模块目标独立改名为 `brutal_base`、`tcp_brutal_base`、`brutal_base`，单独构建加载。测试脚本不替用户安装或替换正式模块。

自检资源回归使用正确的隔离管理构建和命名不存在算法的失败注入构建：`python3 scripts/review-selfcheck.py --success ./tbc-review --failure ./tbc-review-failure --output /tmp/brutal-selfcheck-new`。它在 ARM64/AMD64 上通过只读 BPF 枚举核实三轮成功/失败后的对象及活动规则不变，允许内核最多 3 秒的延迟释放；全局 BPF 枚举应在没有其他 BPF 配置变更时执行。

```bash
# 默认：4 个代表性组合 × 4 算法 × 5 次；每次预热 10 秒、测量 60 秒
sudo python3 scripts/review-benchmark.py --output /tmp/brutal-perf-new
# 明确扩展完整 18 组合；约 7 小时，报告记录实际完成范围
sudo python3 scripts/review-benchmark.py --matrix --output /tmp/brutal-matrix-new
# 更多独立测试；须顺序执行，不能并发污染 CPU/队列
sudo python3 scripts/review-benchmark.py --extended --output /tmp/brutal-500-new
sudo python3 scripts/review-benchmark.py --mixed --scenario 20,80,0 --output /tmp/brutal-mixed-new
sudo python3 scripts/review-benchmark.py --scenario 100,80,0 --drop-mbps 20 --output /tmp/brutal-drop-new
sudo python3 scripts/review-benchmark.py --parallel 8 --quick --output /tmp/brutal-multiflow-new
# 显式配置对照，不更改产品默认值
sudo python3 scripts/review-benchmark.py --scenario 100,80,0 --algorithms brutal_base,brutal_review --cap 100 --output /tmp/brutal-cap-new
sudo python3 scripts/review-benchmark.py --scenario 100,80,0 --algorithms bbr,brutal_review --cap 100 --target-percent 94 --output /tmp/brutal-margin-new
```

服务端在 host init_net，客户端和瓶颈在专用 veth/namespace，只改测试虚拟队列。脚本拒绝占用已有测试标识，输出目录必须为新目录；正常或异常退出都清理本轮网络、连接与临时规则。输出原始接收端 JSON、TCP 快照、带时间戳的 ping、环境、退出状态和汇总；预热期 RTT 样本不计入 p95。`--mixed` 使用标准库应用夹具而非 iperf3，短流记录连接建立到完整响应收完的时间，CPU 数据不可与 iperf3 直接比较。p95 是测量期 ICMP 负载探针 RTT，不是 TCP RTT 分位数。

每场结束后仅对合成测试地址及端口执行 `ss -K`，清除 TCP 关闭尾部；不针对其他 socket。`--continue-on-error` 可用于长矩阵：应用异常场次另写 `failures.json`，不进入有效吞吐统计，但仍完成其余场次；存在失败时总退出码仍为 1。失败原始 stdout/stderr、TCP 和 ping 都保留。性能/内核脚本另写 `cleanup-status.json`，逐项尝试所有清理操作；清理失败会把退出状态改为非零。汇总脚本列出已索引失败及旧夹具未索引的 error 标签，不把失败场次当作零吞吐。失败及补测必须一并报告，不能只挑选成功场次。源码从 Windows 打包时还须核实 `.sh` 文件实际为 LF 并保留 Linux 执行权限，不能只看仓库的 eol 属性。

在没有其他测试占用该网络时，`sudo bash scripts/review-harness-regression.sh /tmp/brutal-harness-new` 可复现故障采集回归：只终止本次创建的临时 iperf3 服务端，核实下一场继续、总状态为失败、原始输出保留且网络资源清理。这是夹具验证，不纳入性能成绩。

测试完成后先按可执行文件路径和参数核实并停止本次隔离管理进程，确认其 socket/BPF 已释放；再依次卸载 `brutal_review`、`brutal_base`。不要强制卸载，不要停止原 `brutal`/canary 服务。核实测试 namespace/虚拟接口/连接消失、正式配置及物理 qdisc 保持原状，保留原始结果和源码目录。

本轮实测结果和未满足/未覆盖事项以 [性能报告](PERFORMANCE_REVIEW.zh.md) 为准。单机网络仿真不能证明真实公网提速比例。保留服务器现有服务和配置，不自动切换生产流量。
