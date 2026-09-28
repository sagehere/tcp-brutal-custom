# Canary 并行 A/B 测试

本分支把 Phase 2 自适应控制器封装成一套**独立 Canary 安装**，可以和现有 2.1.9 baseline 在同一台服务器上同时存在。

## 资源隔离

| 2.1.9 Baseline | Canary |
| --- | --- |
| 内核模块 `brutal` | `brutal_canary` |
| TCP 算法 `brutal` | `brutal_adaptive` |
| DKMS `tcp-brutal-custom` | `tcp-brutal-canary` |
| `/proc/net/tcp_brutal` | `/proc/net/tcp_brutal_canary` |
| `tbc2` | `tbc2-canary` |
| `/etc/tcp-brutal-custom` | `/etc/tcp-brutal-canary` |
| `/var/lib/tcp-brutal-custom` | `/var/lib/tcp-brutal-canary` |
| `/run/tcp-brutal-custom` | `/run/tcp-brutal-canary` |
| `tcp-brutal-custom-*` | `tcp-brutal-canary-*` |

Canary 的 eBPF map/program 也使用独立名称，并且只选择 `brutal_adaptive`。

## Stage 1 对照方式

Stage 1 明确采用**不同本地服务端口**做 A/B：

```text
443  -> 2.1.9 baseline / brutal
8443 -> Phase 2 Canary / brutal_adaptive
```

Canary manager 会检查正式版 `/proc/net/tcp_brutal/ports`，如果端口已经由 baseline 接管，会拒绝添加，避免两个 selector 抢同一个端口。

当前 Stage 1 **不支持同一个 443 端口内部 90/10 分流**。同端口按连接百分比分流属于下一阶段。

## 安装 Canary

在 `feature/canary-side-by-side` 分支源码目录执行：

```bash
sudo ./scripts/install-canary.sh
```

Canary 默认面板端口：`23334`。

例如将 8443 交给 Canary：

```bash
sudo tbc2-canary port add 8443 100
sudo tbc2-canary ports
```

正式版仍照常使用：

```bash
sudo tbc2 port add 443 100
sudo tbc2 ports
```

## 卸载 Canary

保留 Canary 配置和历史：

```bash
sudo /usr/local/lib/tcp-brutal-canary/install-canary.sh --uninstall
```

连配置一起清理：

```bash
sudo /usr/local/lib/tcp-brutal-canary/install-canary.sh --uninstall --purge
```

卸载脚本只处理 Canary 的 DKMS、模块、二进制、systemd 服务和运行目录，不会停止、覆盖或删除正式 2.1.9。

## 更新策略

Canary 自更新功能被明确禁用。测试版本只能从指定 Canary 分支源码通过 `install-canary.sh` 安装，避免误点“更新”后覆盖正式 2.1.9。

## `kr` 实机验收

Ubuntu 24.04.4 arm64 / Linux 6.17 已验证：

- `brutal` 与 `brutal_canary` 可同时加载；
- `tcp_available_congestion_control` 同时出现 `brutal` 与 `brutal_adaptive`；
- 两套 `/proc` 接口同时存在；
- 两个 SockOps selector 可同时挂载；
- baseline 5260 的 accepted socket 使用 `brutal`；
- Canary 5261 的 accepted socket 使用 `brutal_adaptive`；
- Canary 添加 baseline 已占用的 5260 会被拒绝；
- Canary DKMS 安装、服务启动、停止、卸载全过程没有中断 baseline manager 和 baseline 规则；
- Canary 完全卸载后，新建 5260 连接仍然使用 `brutal`。

## 下一阶段

下一阶段将把“不同端口 A/B”升级成统一 selector：

```text
同一个 443
  95% -> brutal
   5% -> brutal_adaptive
```

并支持 95/5、90/10、75/25、50/50 和一键回到 100% baseline。
