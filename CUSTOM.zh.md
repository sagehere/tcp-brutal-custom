# TCP Brutal Custom

基于 [HyNetworks/tcp-brutal](https://github.com/HyNetworks/tcp-brutal) v2.0.1 修改；遵守仓库中的 GPL-3.0 许可证。目标是在 Debian/Ubuntu VPS 上无需修改应用，即可按**本机 TCP 服务端口**选择 Brutal。相同端口的连接共享一个目标速率，IPv4、IPv6 共用。新增或删除规则只影响新连接；修改目标速率会影响已接管连接。

Brutal 只控制本机向对端发送的 TCP 数据。UDP、QUIC、纯路由转发及 Docker 桥接映射端口不在本版范围。输入 Mbps 是目标有效速率，丢包补偿可能让实际发送速率更高，请按实测线路带宽留余量。

## 安装

支持 Debian 12/13、Ubuntu 22.04/24.04，amd64/arm64，root、systemd、cgroup v2、DKMS 和匹配内核头文件。

Release 使用离线 Ed25519 私钥签名。首次安装不再推荐直接执行 main 分支脚本；应先按 [README.zh.md](README.zh.md) 的“验签安装”流程核对公钥指纹、验证 `hashes.txt.sig` 和安装器哈希，再给予 root 权限。

安装成功后，可信发布公钥会固定保存在本机。后续更新只接受该公钥验证通过的 Release，不会从 GitHub 自动替换信任根。

首次安装输出随机管理员密码。配置在 `/etc/tcp-brutal-custom/config.json`；历史库在 `/var/lib/tcp-brutal-custom/history.db`。管理面板默认监听 `http://服务器IP:23333`。面板使用 HTTP 时，密码和会话传输没有 TLS 加密，公网使用应通过反向代理增加 HTTPS 并限制来源 IP。

## 管理

```bash
sudo tbc2                     # 交互菜单
sudo tbc2 status              # 模块、规则、BPF、升级状态
sudo tbc2 port add 443 100    # 443 端口共享 100 Mbps，gain 默认 20=2.0
sudo tbc2 port add 443 80 gain=15
sudo tbc2 port del 443
sudo tbc2 ports
sudo tbc2 panel 0.0.0.0 23334 203.0.113.5
sudo systemctl restart tcp-brutal-custom-web
sudo tbc2 autostart off
sudo tbc2 update              # 仅本机 root/SSH 可触发
sudo tbc2 uninstall
```

Web 面板可以检查是否存在新版本和查看发布说明，但不能触发维护升级。原因是升级过程会断开受管连接并替换内核模块，属于高影响操作。

原有目标 IP 接口 `brutalctl add/del/list/flush` 保持可用；`brutalctl port ...` 转发到定制管理程序。端口规则在连接建立时优先匹配。面板和 SSH 端口不会自动加入接管。

面板展示应发送（不重复的 TCP 数据）、实发送（含重传）和确认字节；重传率为时间窗口内重传字节除以实发送字节。当前连接详情可查看客户端 IP、端口、TCP 状态和拥塞控制算法，不保存 IP 历史。可在面板设置中管理两个服务的开机启动。RTT 为采样均值及最大值。10 秒、1 分钟、1 小时数据分别保留 7 天、90 天、365 天。CSV/JSON 导出包含两种发送量、配置事件和统计间断标记。历史库达到 1 GiB 时会清理旧数据并记录统计空缺。

## 更新安全

更新器先验证 Release 的 Ed25519 签名清单 `hashes.txt.sig`，再按照已签名 `hashes.txt` 校验二进制、DKMS 包和安装器。任一步失败都会中止。

验证完成后，更新在独立 systemd 服务中执行：停止接管并断开受管端口已有 TCP 连接，再切换模块。如果模块仍被其他连接占用，升级停止；切换失败会尝试恢复之前的模块和程序。请在维护窗口执行。

卸载默认保留配置和历史：`sudo /usr/local/lib/tcp-brutal-custom/install.sh --uninstall`；彻底清理需额外加 `--purge`。

## 发布

GitHub Actions 只生成未签名 Release bundle，不直接发布可信 Release。发布者必须在 GitHub 之外持有离线 Ed25519 私钥并执行：

```bash
bash scripts/sign-release.sh /path/to/release-signing-key.pem build
bash scripts/publish-release.sh vX.Y.Z /path/to/release-signing-key.pem build
```

公钥与指纹见 [TRUST.md](TRUST.md)。

## 源码构建

```bash
make dkms-tarball
go build -o tbc2 .
```

模块名和拥塞控制算法名仍为 `brutal`。目标 IP 规则原始接口由上游文档说明。内核模块和 BPF 程序需在实际目标内核上验证；仅编译通过不能证明接管成功。
