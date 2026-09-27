# TCP Brutal Custom

基于 [HyNetworks/tcp-brutal](https://github.com/HyNetworks/tcp-brutal) v2.0.1 修改；遵守仓库中的 GPL-3.0 许可证。目标是让 Debian/Ubuntu VPS 上无需修改应用，即可按**本机 TCP 服务端口**选择 Brutal。相同端口的连接共享一个目标速率，IPv4、IPv6 共用。只有新连接受新增或删除的规则影响，修改目标速率会影响已接管的连接。

Brutal 只控制本机向对端发送的 TCP 数据。UDP、QUIC、纯路由转发及 Docker 桥接映射的端口不在本版范围。输入的 Mbps 是目标有效速率，丢包补偿可能让发送速率高于此值；请按实测线路带宽留余量。

## 安装

支持 Debian 12/13、Ubuntu 22.04/24.04，amd64/arm64，root、systemd、cgroup v2、DKMS 和可用的匹配内核头文件。安装器从本仓库的 GitHub Release 下载带 SHA-256 校验的程序和 DKMS 源码，在目标机编译模块。

```bash
curl -fsSL https://raw.githubusercontent.com/sagehere/tcp-brutal-custom/main/scripts/install.sh -o /tmp/tcp-brutal-custom-install.sh
sudo bash /tmp/tcp-brutal-custom-install.sh
```

首次安装输出随机管理员密码。配置在 `/etc/tcp-brutal-custom/config.json`；历史库在 `/var/lib/tcp-brutal-custom/history.db`。管理面板默认监听 `http://服务器IP:23333`，首次登录后应设置访问 IP 白名单。面板按用户要求使用 HTTP，密码和会话经过网络时没有加密，公网使用时宜通过已有反向代理添加 HTTPS。

## 管理

```bash
sudo tcp-brutal-custom                     # 交互菜单
sudo tcp-brutal-custom status              # 模块、规则、BPF、升级状态
sudo tcp-brutal-custom port add 443 100    # 443 端口共享 100 Mbps，增益默认 20=2.0
sudo tcp-brutal-custom port add 443 80 gain=15
sudo tcp-brutal-custom port del 443
sudo tcp-brutal-custom ports
sudo tcp-brutal-custom panel 0.0.0.0 23334 203.0.113.5
sudo systemctl restart tcp-brutal-custom-web
sudo tcp-brutal-custom autostart off
sudo tcp-brutal-custom update
sudo tcp-brutal-custom uninstall
```

原有目标 IP 接口 `brutalctl add/del/list/flush` 保持可用；`brutalctl port ...` 转发到定制管理程序。端口规则在连接建立时优先匹配。面板和 SSH 端口不会自动加入接管。

面板展示实际发送、确认和重传字节；重传率为时间窗口内重传字节除以总发送字节。RTT 为采样均值及最大值。10 秒、1 分钟、1 小时数据分别保留 7 天、90 天、365 天。CSV/JSON 导出包含配置事件和统计间断标记。历史库达到 1 GiB 时先清理旧数据；清理造成的空缺会记录。

更新在独立的 systemd 服务中执行：先下载并构建新版，之后停止接管并断开受管端口的现有 TCP 连接，再切换模块。如果模块仍被其他连接占用，升级停止；切换失败会尝试恢复之前的模块和程序。请在维护窗口执行。卸载默认保留配置和历史：`sudo /usr/local/lib/tcp-brutal-custom/install.sh --uninstall`；彻底清理需额外加 `--purge`。

## 源码构建

```bash
make dkms-tarball
go build -o tcp-brutal-custom .
```

模块名和拥塞控制算法名仍为 `brutal`。目标 IP 规则的原始接口由上游文档说明。内核模块和 BPF 程序需在实际目标内核上验证；仅编译通过不能证明接管成功。
