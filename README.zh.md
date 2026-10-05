# TCP Brutal Custom

<p align="center">
  <img src="logo.png" width="400" alt="TCP Brutal">
</p>

<p align="center">
  按本机 TCP 服务端口透明接管 Brutal，提供共享速率连接组、Web 管理面板、历史统计、DKMS 安装和更新回滚能力。
</p>

<p align="center">
  <a href="README.md">English</a> ·
  <a href="CUSTOM.zh.md">详细使用说明</a> ·
  <a href="https://github.com/HyNetworks/tcp-brutal">上游 TCP Brutal</a>
</p>

> 本项目基于 [HyNetworks/tcp-brutal](https://github.com/HyNetworks/tcp-brutal) v2.0.1 修改，并继续遵循 GPL-3.0。上游提供 Brutal 拥塞控制算法、目标地址规则和 socket 参数接口；本项目在此基础上增加了**按本机 TCP 服务端口透明接管**、管理面板、历史统计和自动化部署能力。

## 这个项目解决什么问题

目标很直接：

> 在 Debian / Ubuntu VPS 上运行普通 TCP 服务，不修改应用程序，也能按**本机服务端口**自动启用 Brutal。

例如：

```bash
sudo tbc2 port add 443 100
```

之后，新建立到本机 TCP 443 端口的连接会自动切换为 Brutal。

这里的 `100` 表示该端口所有连接共同共享的 **100 Mbps 目标有效速率**。

这并不是每条连接各 100 Mbps：

- 只有一条活跃连接时，它可以使用整个连接组速率；
- 多条活跃连接会动态共享同一个总速率；
- 修改速率后，已经接管的连接会立即使用新值；
- 新增或删除端口规则，只影响之后新建立的连接。

同一端口上的 IPv4 与 IPv6 连接共享同一个连接组。

## 主要特性

- **按本地端口透明接管**：无需修改 Nginx、代理、SSH 或其他 TCP 应用。
- **端口级共享速率**：同一端口的所有受管连接共享一个总目标速率，不会按连接数量倍增。
- **eBPF SockOps 自动选择**：在被动连接建立时，根据本机端口自动切换到 `brutal` 拥塞控制。
- **Web 管理面板**：管理端口、速率、CWND 增益、访问白名单、历史记录和更新。
- **SSH / CLI 管理**：添加/删除端口、查看状态、修改面板、更新、卸载。
- **历史统计**：记录发送、确认、重传流量及 RTT，使用 SQLite 保存。
- **DKMS 安装**：内核模块跟随内核升级重新构建。
- **更新与回滚**：先准备新版本，再在维护窗口切换模块；切换失败时尝试恢复旧版本。
- **兼容上游能力**：保留原有目标 IP 规则和 socket 参数接口。

## 系统要求

当前安装器支持：

- Debian 12 / 13
- Ubuntu 22.04 / 24.04
- amd64 / arm64
- systemd
- cgroup v2
- root 权限
- DKMS
- 与当前内核匹配的内核头文件

本项目同时涉及 Linux 内核模块和 eBPF。**仅仅编译成功，不代表所有内核版本都一定能正确完成端口接管。** 在正式依赖之前，应在实际目标内核上验证。

## 一键安装

普通用户推荐直接使用：

```bash
curl -fsSL https://raw.githubusercontent.com/sagehere/tcp-brutal-custom/main/scripts/bootstrap.sh | sudo bash
```

这条命令并不是把 Release 里的安装器直接交给 root 执行。引导脚本会先：

1. 使用内置的 Ed25519 发布公钥；
2. 核对固定公钥指纹；
3. 验证 `hashes.txt.sig`；
4. 从已签名的 `hashes.txt` 校验 `install.sh` 的 SHA-256；
5. 全部通过后才执行安装器；
6. 安装器内部再对签名清单和下载产物进行第二次验证。

如需固定安装某个已签名版本：

```bash
curl -fsSL https://raw.githubusercontent.com/sagehere/tcp-brutal-custom/main/scripts/bootstrap.sh | \
  sudo env TCP_BRUTAL_RELEASE_TAG=v2.1.11 bash
```

> **首次安装的信任边界：** `bootstrap.sh` 本身仍来自本 GitHub 仓库。如果你的威胁模型包含“首次安装前整个 GitHub 仓库/账号已经被接管”，仍应通过独立可信渠道核对下方公钥指纹后再授予 root 权限。安装成功后公钥会固定在本机，后续普通更新不会重新从 GitHub 建立信任根。

## 验签安装

Release 使用独立的 Ed25519 离线签名密钥，私钥**不存放在 GitHub，也不放入 GitHub Actions Secret**。

首次安装前，请先取得 `keys/release-signing-pub.pem`，并通过**独立于本 GitHub 仓库的可信渠道**核对以下 SHA-256 公钥指纹：

```text
249a5abded1a497f8fe67f4cf5cd8e47d127b9cee2d9b1eac23042b7edfeceff
```

确认信任根后，再在给予 root 权限之前验证 Release：

```bash
base=https://github.com/sagehere/tcp-brutal-custom/releases/latest/download
curl -fsSL "$base/hashes.txt" -o /tmp/hashes.txt
curl -fsSL "$base/hashes.txt.sig" -o /tmp/hashes.txt.sig
curl -fsSL "$base/install.sh" -o /tmp/tcp-brutal-custom-install.sh

openssl pkeyutl -verify -rawin -pubin \
  -inkey keys/release-signing-pub.pem \
  -sigfile /tmp/hashes.txt.sig \
  -in /tmp/hashes.txt

grep ' install.sh$' /tmp/hashes.txt >/tmp/install.check
(cd /tmp && sha256sum -c install.check)
sudo bash /tmp/tcp-brutal-custom-install.sh
```

安装器内部还会再次验证 `hashes.txt.sig`，并对所有下载产物做 SHA-256 校验。安装成功后，可信发布公钥会固定保存在本机；普通在线更新不会从 GitHub 自动替换该信任根。

首次安装完成后会输出随机管理员密码。详细信任模型见 [TRUST.md](TRUST.md)。

默认文件位置：

| 项目 | 路径 |
| --- | --- |
| 配置文件 | `/etc/tcp-brutal-custom/config.json` |
| 历史数据库 | `/var/lib/tcp-brutal-custom/history.db` |
| 管理 Unix Socket | `/run/tcp-brutal-custom/manager.sock` |
| 端口规则 | `/proc/net/tcp_brutal/ports` |
| 目标地址规则 | `/proc/net/tcp_brutal/rules` |

## Web 管理面板

默认地址：

```text
http://服务器IP:23333
```

首次登录后，建议立即设置允许访问的客户端 IP。

> 内置面板默认使用 HTTP。密码和会话在网络传输时不会被 TLS 加密。如果面板暴露在不可信网络上，建议通过 Nginx、Caddy 等反向代理增加 HTTPS，并配合访问控制使用。

面板可查看或管理：

- 已接管端口；
- 当前连接数与客户端 IP 地址；
- 目标 Mbps；
- CWND gain；
- 应发送（不重复数据）/ 实发送（含重传）/ 已确认流量；
- 重传率；
- 平均 RTT 和最大 RTT；
- 历史曲线；
- CSV / JSON 导出；
- 面板监听地址和访问白名单；
- 两项服务的开机启动和管理员密码（至少 8 个 Unicode 字符）；
- 检查新版本和发布说明；真正的维护升级只能由本机 root CLI 发起。

## CLI 使用

本轮修复新增补偿上限、只告警的出口预算和只读诊断，详细兼容性与验证方法见 [稳健优化说明](IMPROVEMENTS.zh.md)。性能结论与未覆盖范围见 [评估报告](PERFORMANCE_REVIEW.zh.md)。

```bash
# 可命名的容量预算；省略预留比例时为 10%，默认没有预算
sudo tbc2 budget set '[{"name":"wan","capacity_mbps":100,"reserve_percent":10}]'
sudo tbc2 port add 443 70 compensation_cap_percent=110 budget=wan
sudo tbc2 budgets
sudo tbc2 diagnose
```

补偿上限范围为整数 `100～125`；`100` 关闭补偿，新增端口省略时为 `125`。旧请求更新现有端口且省略补偿/预算字段时保留原设置。预算不改变网卡队列或限制其他业务流量。

进入交互式管理菜单：

```bash
sudo tbc2
```

常用命令：

```bash
# 查看整体状态
sudo tbc2 status

# 443 端口共享 100 Mbps
# 默认 gain=20，即 2.0x
sudo tbc2 port add 443 100

# 443 端口共享 80 Mbps
# gain=15，即 1.5x
sudo tbc2 port add 443 80 gain=15

# 查看已配置端口
sudo tbc2 ports

# 删除规则
# 已经存在的连接会继续运行直到关闭
sudo tbc2 port del 443

# 修改面板监听地址 / 端口 / 白名单
sudo tbc2 panel 0.0.0.0 23334 203.0.113.5

# 关闭开机自启
sudo tbc2 autostart off

# 更新
sudo tbc2 update  # 仅本机 root / SSH

# 卸载
sudo tbc2 uninstall
```

## 端口接管是怎么工作的

```text
新的 TCP 入站连接
        │
        ▼
cgroup v2 SockOps eBPF
        │
        ├─ 本机端口未配置 ─────► 使用原有 TCP 拥塞控制
        │
        └─ 本机端口已配置
                  │
                  ▼
      setsockopt(TCP_CONGESTION, "brutal")
                  │
                  ▼
          brutal 内核模块
                  │
                  ▼
        本机端口 → 共享连接组
                  │
                  ▼
       Brutal pacing / 丢包补偿
```

eBPF 程序会在被动 TCP 连接建立时运行。

如果该连接的本机监听端口已经加入管理列表，就通过 `setsockopt(TCP_CONGESTION, "brutal")` 自动切换拥塞控制算法。

随后 Brutal 初始化时，会根据本地端口查找对应连接组，并将该 socket 加入同一组。

**本地端口规则的优先级高于上游的目标地址规则。**

## 共享速率的含义

配置的 Mbps 是整个连接组的**目标有效送达速率**，不是严格意义上的物理发送限速器。

Brutal 会进行丢包补偿。

例如，如果目标速率设置为 100 Mbps，而链路存在丢包，为了尽量让对端实际收到接近 100 Mbps，发送端可能会发送超过 100 Mbps 的流量。

因此：

> 不要把目标速率设置得高于链路实际能够承受的水平。

组内带宽也不是简单：

```text
100 Mbps / 当前连接数
```

而是动态调度。

只有一条连接在发送时，它可以占用全部组速率；多条连接同时发送时，它们共享同一个调度时钟。

## 端口规则生命周期

### 新增端口

```bash
sudo tbc2 port add 443 100
```

只会影响之后建立的新连接。

已经存在的 443 TCP 连接不会突然切换到 Brutal。

### 修改速率

再次执行：

```bash
sudo tbc2 port add 443 80
```

会更新原有连接组，因此已经接管的连接会立即使用新的速率。

### 删除端口

```bash
sudo tbc2 port del 443
```

删除后：

- 新连接不再被接管；
- 已经进入 Brutal 连接组的连接继续使用原来的组配置；
- 等这些旧连接关闭后，旧连接组才会完全退出。

## 适用范围与限制

本项目的端口接管机制作用于：

> **由本机实际终止 TCP 连接、并由本机发送数据的 socket。**

当前不适用于：

- UDP；
- QUIC；
- 纯路由转发；
- Docker bridge 端口映射；
- 不在 host network namespace 中的 socket。

例如，如果服务运行在 Docker bridge 网络中，通过宿主机 NAT 映射端口，宿主机看到的并不是一个普通的 host-side passive TCP socket，因此这套本地端口接管逻辑不会按预期工作。

此外，不建议把 Brutal 设置为系统全局默认拥塞控制算法。没有命中规则、也没有应用层主动设置参数的 Brutal socket，会使用模块默认的低速率。

## 上游目标地址规则

本项目仍然保留 TCP Brutal v2 的目标地址规则。

例如：

```bash
sudo brutalctl add 203.0.113.5/32 100
sudo brutalctl list
sudo brutalctl del 203.0.113.5/32
```

它的逻辑是：

```text
目标地址前缀 → Brutal 共享连接组
```

完整说明请参考：

[HyNetworks/tcp-brutal](https://github.com/HyNetworks/tcp-brutal)

如果某个连接同时可以匹配：

- 本地端口规则
- 目标 IP 规则

则**本地端口规则优先**。

## 更新与维护

```bash
sudo tbc2 update
```

Web 面板只能检查版本和查看发布说明，不能启动维护升级。会执行断流和内核模块替换的更新动作，只允许本机 root 通过管理 Unix Socket 发起，例如 SSH 登录后执行上面的命令。

在停止任何服务之前，更新器会先使用本机固定的 Ed25519 公钥验证 `hashes.txt.sig`，再按照已签名的 `hashes.txt` 校验每个下载产物。缺少签名或验签失败时会直接中止。

通过验证后才进入维护流程：

1. 准备新版本；
2. 停止管理服务；
3. 停止新的 Brutal 自动接管，但不主动断开现有 TCP 连接；等待旧连接自然结束后再切换模块；
4. 卸载当前 `brutal` 模块；
5. 安装并切换新的 DKMS 模块和程序；
6. 重新加载模块并启动服务。

如果内核模块仍被其他连接占用，更新会中止；如果切换过程中失败，脚本会尝试恢复之前的程序和模块。

建议始终在维护窗口执行更新。

## 卸载

普通卸载：

```bash
sudo tbc2 uninstall
```

默认保留：

- 配置文件；
- 历史数据库。

如果希望彻底删除：

```bash
sudo /usr/local/lib/tcp-brutal-custom/install.sh --uninstall --purge
```

## 从源码构建

内核模块：

```bash
make
```

生成 DKMS 源码包：

```bash
make dkms-tarball
```

管理程序：

```bash
go build -o tbc2 .
```

上游兼容的 `brutalctl`：

```bash
make -C tools
```

常用检查：

```bash
make format-check
CGO_ENABLED=0 go test ./...
```

## 目录结构

| 文件 | 作用 |
| --- | --- |
| `brutal_cc.c` | Brutal 拥塞控制、pacing 与连接组调度 |
| `brutal_ports.c` | 本地 TCP 端口 → Brutal 连接组 |
| `brutal_rules.c` | 上游目标地址前缀规则 |
| `brutal_sockopt.c` | Brutal socket 参数和应用层连接组 |
| `selector_linux.go` | cgroup SockOps eBPF 端口选择器 |
| `manager.go` | Root 管理进程、API、端口控制和统计 |
| `main.go` | CLI、配置和服务入口 |
| `history.go` | SQLite 历史记录、聚合与保留策略 |
| `web/` | 内嵌 Web 管理面板 |
| `scripts/install.sh` | 安装、更新、回滚、卸载 |
| `tools/brutalctl.c` | 上游兼容 `brutalctl` |

## 与上游 TCP Brutal 的关系

本项目建立在 [HyNetworks/tcp-brutal](https://github.com/HyNetworks/tcp-brutal) 基础之上。

上游实现了 Hysteria Brutal 拥塞控制在 Linux TCP 中的核心能力。

本项目主要增加的是面向服务器运维的功能：

- 按**本机服务端口**透明启用 Brutal；
- 端口级共享连接组；
- eBPF 自动选择；
- Web / CLI 管理；
- 历史统计；
- DKMS 安装；
- Release 驱动的更新和回滚流程。

更详细的运维说明请参阅：

[CUSTOM.zh.md](CUSTOM.zh.md)

### 自动跟踪上游

`.github/workflows/upstream-sync.yml` 每天检查一次 `HyNetworks/tcp-brutal:master`，也可以从 Actions 手动运行。

发现上游新增提交后，workflow 会：

1. 创建 `upstream-sync/<上游SHA>` 分支；
2. 使用标准 Git merge 保留上游提交历史；
3. 只有在无冲突时才 push 同步分支；
4. 自动创建指向 `main` 的 PR；
5. 让现有 PR CI 检查格式、Go 测试和 Shell 语法。

它**不会自动合并，也不会自动发布 Release**。如果出现冲突，Action 会失败并列出冲突文件，不会用 `ours` / `theirs` 自动覆盖。涉及内核或 eBPF 的上游变化仍需人工审查，并在实际目标内核上验证。

## Release 签名与供应链

GitHub Actions 会先构建**未签名的待发布产物**并上传为 workflow artifact。私钥始终留在 GitHub 之外；对 `hashes.txt` 完成离线签名后，独立的发布 workflow 只接收公开签名，重新核对 staged artifact 的字节和签名，验证通过后才创建 tag 和 Release。

正式发布必须在 GitHub 之外使用离线 Ed25519 私钥签名：

```bash
bash scripts/sign-release.sh /path/to/release-signing-key.pem build
bash scripts/publish-release.sh vX.Y.Z /path/to/release-signing-key.pem build
```

安装器和更新器要求 Release 中存在有效的 `hashes.txt.sig`。已安装机器会固定保存可信公钥，普通更新不能静默替换信任根；密钥轮换必须走显式迁移流程。2026-09-28 因原离线私钥遗失已完成一次正式轮换；已有安装必须先执行显式迁移脚本，再继续更新。

详细说明见 [TRUST.md](TRUST.md)。

## 许可证

GPL-3.0，详见 [LICENSE](LICENSE)。

上游 TCP Brutal 代码以及本项目的修改版本均继续受 GPL-3.0 和对应署名要求约束。


## 无感维护行为

升级和卸载采用“先排空、后切换”的维护方式。系统会先停止新的自动 Brutal 接管，但**不会主动断开已有 TCP 连接**；已经使用 Brutal 的连接会继续运行，直到自然关闭。

排空期间，新建立且原本应匹配 Brutal 的连接会临时使用系统默认 TCP 拥塞控制；旧模块不再被任何 socket 使用后，系统自动完成模块切换，并恢复原有端口规则和目标 IP 规则。

`tbc2 uninstall` 会启动后台 systemd 卸载任务并立即返回，因此即使当前 SSH 会话本身正在使用 Brutal，也不会出现“卸载等待 SSH 断开，而 SSH 又等待卸载命令返回”的死锁。

如果某个应用存在超长连接，或者持续主动执行 `TCP_CONGESTION=brutal` 创建新 socket，排空会一直保持等待，直到这些连接结束。


### 中文交互菜单与密码重置

直接执行 `sudo tbc2` 会进入中文交互菜单。Web 管理面板的“面板设置”中既可以手动修改登录密码，也可以点击“重置登录密码”生成新的高强度随机密码。重置成功后，所有已登录会话会立即失效，新密码只在本次重置响应中显示一次，请立即保存。


### 命令行面板管理

中文交互菜单中的面板相关功能统一归口到“面板管理”子菜单：

- 面板网络设置
- 查看当前登录密码
- 重置登录密码
- 修改登录密码

“查看当前登录密码”仅允许本机 root CLI 使用，Web API 不提供读取当前密码的接口。新版本会把当前有效密码额外保存到 root-only、权限为 `0600` 的本地文件，同时继续使用 Argon2 哈希进行登录认证。由旧版本升级而来的安装只保存了不可逆哈希，因此历史密码无法恢复；第一次查看时会提示先重置一次，之后即可查看当前有效密码。备份/恢复会同步保存该密码记录并校验其与 Argon2 哈希一致。
