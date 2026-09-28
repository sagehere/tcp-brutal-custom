# 同端口 Canary A/B 分流

Stage 2 在并存 Canary 基础上加入**同一个服务端口、按 TCP 连接百分比分流**。

## 命令

创建实验：

```bash
sudo tbc2-canary ab add 443 100 5
```

表示：

- 服务端口：443
- 目标速率：100 Mbps
- 5% 新连接使用 Phase 2 Canary
- 95% 新连接使用 2.1.9 baseline

动态修改比例：

```bash
sudo tbc2-canary ab set 443 10
sudo tbc2-canary ab set 443 25
sudo tbc2-canary ab set 443 50
sudo tbc2-canary ab set 443 0
```

查看实验：

```bash
sudo tbc2-canary ab list
```

删除：

```bash
sudo tbc2-canary ab del 443
```

## 分流方式

统一 SockOps selector 使用：

```text
bpf_get_socket_cookie(socket) % 100
```

得到每条 TCP 连接稳定的 0–99 桶。

桶值小于 Canary 百分比：

```text
TCP_CONGESTION=brutal_adaptive
```

否则：

```text
TCP_CONGESTION=brutal
```

比例调整只影响**之后新建立的连接**。已经建立的 TCP 不会在连接中途更换算法。

## 两套内核组

A/B manager 会在同一个实验端口上同时创建：

```text
/proc/net/tcp_brutal/ports
/proc/net/tcp_brutal_canary/ports
```

中的同速率/同 gain group，然后由统一 selector 为每个新连接选择算法。

实验端口不能继续由正式 2.1.9 manager 的持久化配置管理；如果 baseline 配置仍拥有该端口，Canary 会拒绝接管，避免两个控制面同时改同一个端口。

## 一键回退

```bash
sudo tbc2-canary ab set 443 0
```

之后所有**新连接**立即全部回到 `brutal`。

已经存在的 Canary 连接不会被强制断开，而是自然结束。因此 0% 是安全的紧急回退状态。

## `kr` 实机验收

实验端口 5270，每档创建 200 条新 TCP 连接，并读取服务端 accepted socket 的 `TCP_CONGESTION`：

| Canary 配置 | baseline | Canary |
| ---: | ---: | ---: |
| 0% | 200 | 0 |
| 5% | 190 | 10 |
| 50% | 100 | 100 |
| 100% | 0 | 200 |

5% 实测正好是 10/200，50% 正好是 100/200。

### 已有连接不变化

先以 0% 建立第一条连接，然后保持连接不断，把比例改为 100%，再建立第二条连接：

```text
FIRST=brutal
FIRST_AFTER=brutal
SECOND=brutal_adaptive
```

证明修改比例不会改变正在工作的 TCP。

### 持久化与删除

`ab add 5270 100 5` 后重启 Canary manager，`ab list` 仍恢复 5% 实验。

执行 `ab del 5270` 后，baseline 和 Canary 两边的内核 port group 都变为 inactive，同时配置中的实验记录被删除。

## 建议上线顺序

```text
0% -> 5% -> 10% -> 25% -> 50%
```

每一步比较 baseline/Canary 两组的：

- goodput
- retrans
- RTT
- 连接失败率
- 应用层成功率

确认稳定后再扩大比例。

selector 会分别统计每个实验端口的 baseline / Canary / failure 新连接数量。
