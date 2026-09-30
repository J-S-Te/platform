#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/deploy.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/registry-network-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
docker() {
  case "$*" in
    *'{{.HostConfig.NetworkMode}}'*) echo bridge ;;
    'network inspect bridge') echo '[{"Options":{"com.docker.network.bridge.name":"docker0"},"IPAM":{"Config":[{"Subnet":"172.17.0.0/16","Gateway":"172.17.0.1"}]}}]' ;;
    'image inspect registry:2.8.3'|'container inspect uip-offline-registry') return 0 ;;
    *'{{.State.Running}}'*) echo true ;;
    *) return 97 ;;
  esac
}
ip() {
  case "$*" in
    '-d -j link show dev docker0') echo '[{"flags":["UP"],"linkinfo":{"info_kind":"bridge"}}]' ;;
    '-4 -j addr show dev docker0')
      if [[ "$scenario" == conflict ]]; then echo '[{"addr_info":[{"family":"inet","local":"192.0.2.1","prefixlen":24}]}]'
      elif [[ -e "$test_root/address" ]]; then echo '[{"addr_info":[{"family":"inet","local":"172.17.0.1","prefixlen":16}]}]'
      else echo '[{"addr_info":[]}]'; fi ;;
    'address add 172.17.0.1/16 dev docker0') touch "$test_root/address"; echo address >> "$test_root/actions" ;;
    '-4 -j route show exact 172.17.0.0/16')
      if [[ "$scenario" == route-conflict ]]; then echo '[{"dev":"ens33","gateway":"192.168.3.1"}]'
      elif [[ -e "$test_root/route" ]]; then echo '[{"dev":"docker0","prefsrc":"172.17.0.1"}]'
      else echo '[]'; fi ;;
    'route add 172.17.0.0/16 dev docker0 src 172.17.0.1') touch "$test_root/route"; echo route >> "$test_root/actions" ;;
    *) return 98 ;;
  esac
}
curl() { [[ "$scenario" != unavailable ]]; }
sleep() { :; }
# A test-only root predicate lets non-root development hosts exercise repair.
eval "$(declare -f ensure_registry_bridge | sed 's/\$EUID/0/g')"
scenario=healthy
ensure_registry
[[ -e "$test_root/address" && -e "$test_root/route" ]]
ensure_registry
[[ "$(wc -l < "$test_root/actions" | tr -d ' ')" == 2 ]]
scenario=conflict
if (ensure_registry) > "$test_root/conflict.log" 2>&1; then exit 1; fi
grep -q '拒绝覆盖' "$test_root/conflict.log"
scenario=route-conflict
if (ensure_registry) > "$test_root/route-conflict.log" 2>&1; then exit 1; fi
grep -q '冲突路由' "$test_root/route-conflict.log"
scenario=unavailable
if (ensure_registry) > "$test_root/unavailable.log" 2>&1; then exit 1; fi
grep -q '已停止镜像推送' "$test_root/unavailable.log"
printf 'offline registry network regression: PASS\n'
