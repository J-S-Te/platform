#!/usr/bin/env bash
# Offline registry bridge/readiness helpers. Sourced by deploy.sh.

ensure_registry_bridge() {
  # 以 Docker 元数据为权威，绝不假设 docker0 或 172.17.0.0/16。
  [[ "$(docker inspect -f '{{.HostConfig.NetworkMode}}' uip-offline-registry)" == bridge ]] || return 0
  local metadata bridge subnet gateway prefix addresses link routes
  need ip; need jq
  metadata="$(docker network inspect bridge)" || die '无法读取离线仓库 bridge 配置'
  bridge="$(jq -er '.[0].Options["com.docker.network.bridge.name"]' <<<"$metadata")" || die 'Docker bridge 接口未配置'
  subnet="$(jq -er '[.[0].IPAM.Config[] | select(.Subnet | contains(":" ) | not)] | if length == 1 then .[0].Subnet else error("ambiguous IPv4 subnet") end' <<<"$metadata")" || die 'Docker bridge IPv4 网段不唯一'
  gateway="$(jq -er --arg subnet "$subnet" '.[0].IPAM.Config[] | select(.Subnet == $subnet) | .Gateway' <<<"$metadata")" || die 'Docker bridge IPv4 网关缺失'
  [[ "$bridge" =~ ^[a-zA-Z0-9_.-]+$ && "$subnet" =~ ^[0-9.]+/[0-9]+$ && "$gateway" =~ ^[0-9.]+$ ]] || die 'Docker bridge 配置格式异常'
  prefix="${subnet##*/}"
  link="$(ip -d -j link show dev "$bridge")" || die "Docker bridge 接口不存在：$bridge"
  jq -e 'length == 1 and .[0].linkinfo.info_kind == "bridge"' <<<"$link" >/dev/null || die "拒绝修改非 bridge 接口：$bridge"
  addresses="$(ip -4 -j addr show dev "$bridge")" || die "无法读取桥地址：$bridge"
  if ! jq -e --arg gateway "$gateway" --argjson prefix "$prefix" 'any(.[].addr_info[]?; .local == $gateway and .prefixlen == $prefix)' <<<"$addresses" >/dev/null; then
    jq -e '[.[].addr_info[]? | select(.family == "inet")] | length == 0' <<<"$addresses" >/dev/null || die "桥 $bridge 已有其他 IPv4 地址，拒绝覆盖；请核对 Docker 与宿主网络配置"
    [[ "$EUID" -eq 0 ]] || die "桥 $bridge 缺少 $gateway/$prefix，需要 root 恢复后重试"
    printf '恢复 Docker bridge 缺失地址：%s → %s/%s\n' "$bridge" "$gateway" "$prefix"
    ip address add "$gateway/$prefix" dev "$bridge" || die '恢复 Docker bridge 地址失败'
  fi
  if ! jq -e '.[0].flags | index("UP") != null' <<<"$link" >/dev/null; then
    ip link set dev "$bridge" up || die '启动 Docker bridge 接口失败'
  fi
  routes="$(ip -4 -j route show exact "$subnet")" || die '无法读取 Docker bridge 路由'
  if ! jq -e --arg bridge "$bridge" --arg gateway "$gateway" 'any(.[]; .dev == $bridge and .prefsrc == $gateway and (.gateway == null))' <<<"$routes" >/dev/null; then
    [[ "$(jq 'length' <<<"$routes")" == 0 ]] || die "Docker 网段 $subnet 存在冲突路由，拒绝覆盖"
    ip route add "$subnet" dev "$bridge" src "$gateway" || die '恢复 Docker bridge 路由失败'
  fi
}

ensure_registry() {
  need curl
  docker image inspect registry:2.8.3 >/dev/null 2>&1 || die "请先导入公共基础设施镜像包"
  if ! docker container inspect uip-offline-registry >/dev/null 2>&1; then
    docker run -d --name uip-offline-registry --restart unless-stopped -p 127.0.0.1:5000:5000 -v uip-offline-registry-data:/var/lib/registry registry:2.8.3 >/dev/null
  elif [[ "$(docker inspect -f '{{.State.Running}}' uip-offline-registry)" != true ]]; then
    docker start uip-offline-registry >/dev/null
  fi
  ensure_registry_bridge
  local attempt
  for attempt in {1..15}; do
    if curl --noproxy '*' --fail --silent --connect-timeout 2 --max-time 3 "http://$registry_address/v2/" >/dev/null; then
      printf '离线镜像仓库可达性检查通过：http://%s/v2/\n' "$registry_address"
      return 0
    fi
    sleep 1
  done
  die "离线镜像仓库不可达：http://$registry_address/v2/；已停止镜像推送，请检查 Docker bridge、防火墙及仓库日志"
}

# 使用 containerd 镜像存储的 Docker 可能让 docker image inspect .Id 返回镜像索引摘要，
# 而格式 2 包刻意记录 images.tar 中 linux/amd64 镜像配置摘要。因此仅凭 .Id 不匹配不能拒绝正确加载的包。
# 使用私有临时别名重新导出确切标签，再以构建器相同的严格归档解析器解析配置内容。此过程仍拒绝绑定到不同镜像内容的标签，不放宽包摘要或平台检查。
