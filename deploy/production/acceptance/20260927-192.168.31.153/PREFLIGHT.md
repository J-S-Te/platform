# 192.168.31.153 验收预检记录

## 记录范围

- 日期：2026-09-27
- 阶段：首次连接后的只读预检及脱敏源码归档接收
- 未记录 SSH 密码、应用密码、Token、私钥或完整环境变量
- 未操作 192.168.3.38

## 宿主机

| 项目 | 实测结果 |
| --- | --- |
| 操作系统 | Ubuntu 26.04.1 LTS |
| 内核 | Linux 7.0.0-34-generic |
| 架构 | x86_64 / 64 bit |
| 虚拟化 | VMware |
| CPU | 4 vCPU |
| 内存 | 3.3 GiB，可用约 2.6 GiB |
| Swap | 3.8 GiB |
| 根分区 | ext4，约 97 GiB，可用约 85 GiB |
| inode | 可用约 6.1M |
| 时区 | Etc/UTC，NTP 已同步 |
| 主网卡 | ens33，192.168.31.153/24 |
| 默认路由 | 192.168.31.1 |

## Docker

| 项目 | 实测结果 |
| --- | --- |
| Docker Engine | 29.7.2，linux/amd64 |
| Docker Compose | 5.5.0 |
| Buildx | 0.36.1 |
| 存储驱动 | overlayfs |
| Docker Root Dir | /var/lib/docker |
| 业务容器 | 无 |
| Docker 命名卷 | 无 |
| 自定义 Docker 网络 | 无 |

仅有默认 `bridge` / `host` / `none` 网络。Docker 服务处于 active。

## 网络与端口

- TCP 仅确认 SSH `0.0.0.0:22` / `[::]:22` 对外监听。
- UFW systemd unit 存在，但 `ufw status` 为 inactive；firewalld 为 inactive。
- Docker daemon 配置了 `docker.m.daocloud.io` 和 `docker.xuanyuan.me` 镜像加速。
- `registry-1.docker.io` 和 `proxy.golang.org` 的直连 HTTPS 超时；Docker 通过已配置镜像加速成功拉取 `linux/amd64` `alpine:3.21`。
- `registry.npmjs.org` HTTPS 实测可达。

## 现场保护结论

- 未发现 `/opt/unified-identity-platform`、旧交付目录、业务容器、命名卷或自定义网络。
- 目前符合“空白验收机”的 Docker 状态，未删除任何原有资源。
- 主要容量风险是 3.3 GiB 内存。可以继续串行构建和最小平台验证，但全量子系统同时运行可能触发 OOM，不得将硬件容量不足通过关闭健康检查规避。

## 源码交付预处理

- 本地生成的脱敏源码归档：`uip-source-20260927.tar.gz`
- SHA-256：`29f37bc1ab86a3b3d496ba6f25cc9a15493385d5c277e4f25387ef71ad6d146f`
- 传输前排除：`.git`、旧制品、构建缓存、`node_modules`、`dist`、`data`、真实 `.env`、PEM/私钥及本地已编译二进制。
- 服务器端校验摘要一致，路径为 `/root/uip-acceptance/source`。

## 当前门禁

- 继续构建前需先同步本轮文档/构建脚本最终修改。
- 构建时不能依赖 `proxy.golang.org` 直连；应优先使用已审核 `vendor/` 或明确且可记录的 Go 代理。
- 全系统业务验收需根据实际内存消耗判断是否被硬件阻断。
- 宿主机重启不在本轮授权范围。
