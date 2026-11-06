#!/usr/bin/env bash
# 平台发布证明的 Keycloak HTTP 门禁回归（bin/delivery-runtime.sh: require_keycloak_http_ready）。
#
# 背景一（2026-11-05 现场事故）：keycloak-db 重建后 Keycloak 容器没换，仍接受 9000/8080 的 TCP
# 连接，但请求处理已被卡死（Hikari 陈旧连接 + Infinispan clientscopes 缓存）。容器状态和
# healthcheck 都还是 healthy，交付证明因此漏检，直到 platform-worker 因 Keycloak admin API
# 调用失败而反复崩溃。
#
# 背景二（P0 回归）：门禁曾经改为探测 PUBLIC_KEYCLOAK_ISSUER（= PUBLIC_SSO_ORIGIN/realms/<realm>），
# 而公开入口由 frontend nginx 容器发布，阶段 B（平台发布证明）又先于阶段 C（发布前端）执行，
# 于是全新安装必然在阶段 B 失败、--resume 反复重试。门禁必须直连 Keycloak 自己发布到宿主的
# 回环端口，本文件用结构断言固定住这一点。
#
# 背景三（本轮 P3 回归）：门禁的地址来源必须是“已解析的 compose 模型”，不能直接拼 env_get 的
# 裸值——docker compose 会对 .env 做引号/行内注释/空白归一化，`"0.0.0.0"`、`0.0.0.0 `、
# `0.0.0.0 # 备注` 都能正常发布，直接拼进 URL 却会让 curl 拒绝，把健康部署判成“Hikari 卡死”。
#
# 背景四（本轮 P3 回归）：tenant realm 由 platform-worker 启动期 bootstrap（EnsureKeycloakBroker）
# 创建，而 platform-worker 的 healthcheck 只是 PID1 进程探针；KEYCLOAK_MANAGEMENT_ENABLED=false
# 或 bootstrap 慢/失败时 realm 会一直 404。门禁因此拆成两级：master realm 必须是 200（卡死检测），
# tenant realm 的 404 只做有界重试后警告（不阻断发布）。
#
# 装配方式与 test-verify-readiness.sh 相同：放真实脚本 + 替身 curl/jq/compose，判据为
# “真的取到含 issuer 的 OIDC 发现文档才算通过”，其余一律失败并打印中文诊断；全程无网络、无 docker。
#
# 需要 bash 4 以上（与部署脚本一致；macOS 自带 3.2 请用 bash:5 容器执行）。
set -Eeuo pipefail
if [[ "${BASH_VERSINFO[0]:-0}" -lt 4 ]]; then
  printf '本用例需要 bash 4 以上（当前 %s）\n' "${BASH_VERSION:-unknown}" >&2
  exit 1
fi

source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/keycloak-probe-test.XXXXXX")"
trap 'rm -r -- "$test_root"' EXIT
mkdir -p "$test_root/deploy/bin" "$test_root/deploy/manifests" "$test_root/deploy/packages"
cp "$source_dir/bin/deploy.sh" "$source_dir/bin/compose-scope.sh" "$source_dir/bin/subsystem-runtime.py" \
  "$source_dir/bin/start-enabled.sh" "$source_dir/bin/project-background.sh" \
  "$source_dir/bin/provisioner-config-refresh.sh" "$source_dir/bin/offline-configure.sh" \
  "$source_dir/bin/offline-package-metadata.sh" "$source_dir/bin/public-transport.sh" \
  "$source_dir/bin/delivery-runtime.sh" "$source_dir/bin/delivery-runtime.py" \
  "$test_root/deploy/bin/"
cp "$source_dir"/bin/deploy-{common,import,subsystem,license,doctor,package,status,verify,menu,registry}.sh \
  "$test_root/deploy/bin/"
cp "$source_dir/docker-compose.yml" "$test_root/deploy/"
cp -R "$source_dir/subsystems.d" "$test_root/deploy/"
: >"$test_root/deploy/.release.env"
chmod 600 "$test_root/deploy/.release.env"

# 运行配置是门禁的 .env 来源：每个用例只改这个真实文件，不靠环境里预置的变量。
write_runtime_config() {
  {
    printf 'PUBLIC_ACCESS_MODE=ip\n'
    printf 'PUBLIC_PLATFORM_HOST=192.0.2.10\n'
    printf 'PUBLIC_SSO_HOST=192.0.2.10\n'
    printf 'PUBLIC_HTTP_PORT=8081\n'
    printf '%s' "${1:-}"
  } >"$test_root/deploy/.env"
  chmod 600 "$test_root/deploy/.env"
}

# 默认（无 KEYCLOAK_* 键）走代码内置默认：127.0.0.1:18090/realms/basic-platform。
write_runtime_config ''
source "$test_root/deploy/bin/deploy.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# 有界重试的 5 秒间隔在测试里不真等；重试次数仍由门禁自己固定为 6 次（下面会断言）。
sleep() { :; }

compose_model=''
compose_calls="$test_root/compose.log"
: >"$compose_calls"
# 只读替身：默认不给模型（模拟“拿不到 compose 模型”），需要时用 compose_model 注入。
compose() {
  printf '%s\n' "$*" >>"$compose_calls"
  [[ "$1" == config && "${2:-}" == --format && "${3:-}" == json ]] || return 0
  [[ -n "$compose_model" ]] || return 1
  printf '%s\n' "$compose_model"
}

curl_log="$test_root/curl.log"
: >"$curl_log"
# 替身 curl：按 URL 返回给定响应体 + --write-out 追加的 3 位状态码，并模拟 curl 退出码。
# 真实 curl 的 URL 是最后一个参数，门禁必须保持这个调用顺序（本替身也用 "${*: -1}" 取 URL）。
master_status=0
master_code=200
master_body='{"issuer":"http://127.0.0.1:18090/realms/master"}'
tenant_status=0
tenant_code=200
tenant_body='{"issuer":"http://127.0.0.1:18090/realms/basic-platform"}'
reset_curl() {
  master_status=0; master_code=200; master_body='{"issuer":"http://127.0.0.1:18090/realms/master"}'
  tenant_status=0; tenant_code=200; tenant_body='{"issuer":"http://127.0.0.1:18090/realms/basic-platform"}'
  : >"$curl_log"
}
curl() {
  printf '%s\n' "$*" >>"$curl_log"
  local url="${*: -1}" status=0 code='' body=''
  case "$url" in
    */realms/master/.well-known/*) status="$master_status"; code="$master_code"; body="$master_body" ;;
    */realms/*/.well-known/*) status="$tenant_status"; code="$tenant_code"; body="$tenant_body" ;;
    *) fail "门禁发起了非 OIDC 发现文档的探测：$url" ;;
  esac
  [[ -z "$body" ]] || printf '%s' "$body"
  [[ -z "$code" ]] || printf '\n%s' "$code"
  return "$status"
}

# 替身 jq：只实现本门禁用到的 `jq -er '.issuer'`，并保持 -e 的“空值/缺失即非零”语义。
jq() {
  local filter="${*: -1}" document issuer
  document="$(cat)"
  [[ "$filter" == ".issuer" ]] || return 1
  issuer="$(printf '%s' "$document" |
    sed -n 's/.*"issuer"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  [[ -n "$issuer" ]] || return 1
  printf '%s\n' "$issuer"
}

assert_probe_url() { # $1 expected url, $2 label
  grep -Fq -- "$1" "$curl_log" || fail "$2：门禁没有探测预期地址 $1；curl 实际参数：$(cat "$curl_log")"
}

assert_success() { # $1 log file, $2 label
  require_keycloak_http_ready >"$1" 2>&1 || fail "$2：门禁判为失败：$(cat "$1")"
  grep -Fq 'Keycloak OIDC 发现文档可用' "$1" || fail "$2：成功证据缺失（操作员看不到发现文档已核验）"
}

assert_wedged_failure() { # $1 log file, $2 label, $3 expected url
  require_keycloak_http_ready >"$1" 2>&1 && fail "$2：门禁把不可用的 Keycloak 判为就绪"
  grep -Fq 'Keycloak 未提供可用的 OIDC 发现文档' "$1" || fail "$2：缺少“发现文档不可用”的中文诊断"
  grep -Fq -- "$3" "$1" || fail "$2：诊断没有写明实际探测的 URL（$3）"
  grep -Fq '卡死' "$1" || fail "$2：诊断没有给出“请求处理卡死”这一现场假设"
  grep -Fq 'Hikari' "$1" || fail "$2：诊断没有点出 2026-11-05 事故的陈旧连接池形态"
  grep -Fq 'Infinispan' "$1" || fail "$2：诊断没有点出 Infinispan clientscopes 卡死形态"
  grep -Fq '重启 keycloak' "$1" || fail "$2：诊断没有给出可操作的修复建议"
}

# 门禁必须真的挂在平台发布证明上，否则本文件测的只是孤立函数。
release_body="$(declare -f delivery_release_matches)"
grep -Fq 'verify_current_services' <<<"$release_body" ||
  fail '平台发布证明缺少现有服务健康门禁：用例上下文已失真'
grep -Fq 'require_keycloak_http_ready' <<<"$release_body" ||
  fail 'delivery_release_matches 的平台分支没有调用 Keycloak HTTP 门禁'

# 结构断言（P0 回归）：探针地址只能来自 Keycloak 自己的宿主端口与 realm 配置，
# 一旦有人把它改回公开入口，全新安装又会因为前端还没发布而永久失败。
probe_body="$(declare -f require_keycloak_http_ready)"
for forbidden in PUBLIC_KEYCLOAK_ISSUER PUBLIC_SSO_ORIGIN; do
  if grep -Fq "$forbidden" <<<"$probe_body"; then
    fail "门禁探针又引用公开入口 $forbidden：全新安装阶段该入口由前端发布，必然死锁"
  fi
done
for required in KEYCLOAK_BIND_ADDRESS KEYCLOAK_HTTP_PORT KEYCLOAK_REALM; do
  grep -Fq "$required" <<<"$probe_body" ||
    fail "门禁探针没有从运行配置读取 $required，无法在平台阶段自证可用"
done
# 结构断言（Y2/Y3）：端点必须优先来自已解析的 compose 模型，且 master realm 是硬性要求。
for required_call in keycloak_published_endpoint 'compose config' 'realms/master' 'KEYCLOAK_MANAGEMENT_ENABLED'; do
  grep -Fq "$required_call" <<<"$probe_body" ||
    fail "门禁实现没有体现 ${required_call}（端点来源或两级判定缺失）"
done

# (a) 健康：master 与 tenant realm 都是 200 + 含 issuer => 通过，地址来自运行配置默认值（回环端口）。
reset_curl
assert_success "$test_root/ready.log" '默认回环地址'
assert_probe_url 'http://127.0.0.1:18090/realms/master/.well-known/openid-configuration' '默认 master 探针'
assert_probe_url 'http://127.0.0.1:18090/realms/basic-platform/.well-known/openid-configuration' '默认 tenant 探针'
for flag in --fail --silent --show-error --connect-timeout --max-time --write-out; do
  grep -Fq -- "$flag" "$curl_log" || fail "门禁的 curl 缺少 $flag（无法判定 HTTP 失败、有界超时或状态码）"
done

# (b) KEYCLOAK_BIND_ADDRESS=0.0.0.0 是“监听所有接口”，客户端必须换成 127.0.0.1 才能连上。
write_runtime_config 'KEYCLOAK_BIND_ADDRESS=0.0.0.0
KEYCLOAK_HTTP_PORT=18090
KEYCLOAK_REALM=basic-platform
'
reset_curl
assert_success "$test_root/wildcard.log" '通配监听地址'
assert_probe_url 'http://127.0.0.1:18090/realms/basic-platform/.well-known/openid-configuration' '通配地址归一化'
if grep -Fq -- 'http://0.0.0.0:' "$curl_log"; then
  fail "门禁把通配地址当成客户端地址：$(cat "$curl_log")"
fi

# (c) 运行配置里的自定义端口与 realm 必须被采用（不能写死默认值）。
write_runtime_config 'KEYCLOAK_BIND_ADDRESS=127.0.0.1
KEYCLOAK_HTTP_PORT=19091
KEYCLOAK_REALM=tenant-realm
'
reset_curl
tenant_body='{"issuer":"http://127.0.0.1:19091/realms/tenant-realm"}'
assert_success "$test_root/custom.log" '自定义端口/realm'
assert_probe_url 'http://127.0.0.1:19091/realms/tenant-realm/.well-known/openid-configuration' '自定义端口/realm'

# (d) Y2 回归：docker compose 会对 .env 做归一化，env_get 的裸值不能直接进 URL。
# 这三种写法在真实部署里都能正常发布，门禁必须同样解释成 127.0.0.1:19091。
write_runtime_config 'KEYCLOAK_BIND_ADDRESS="127.0.0.1"
KEYCLOAK_HTTP_PORT="19091"
KEYCLOAK_REALM="tenant-realm"
'
reset_curl
assert_success "$test_root/quoted.log" '引号包裹的取值'
assert_probe_url 'http://127.0.0.1:19091/realms/tenant-realm/.well-known/openid-configuration' '引号归一化'
if grep -Fq 'http://"' "$curl_log"; then
  fail "引号没有被剥离，URL 里出现 http://\"：$(cat "$curl_log")"
fi

write_runtime_config "$(printf 'KEYCLOAK_BIND_ADDRESS=127.0.0.1 \nKEYCLOAK_HTTP_PORT=19091\t\nKEYCLOAK_REALM=tenant-realm \n')"
reset_curl
assert_success "$test_root/trailing-space.log" '行尾空白'
assert_probe_url 'http://127.0.0.1:19091/realms/tenant-realm/.well-known/openid-configuration' '行尾空白归一化'

write_runtime_config 'KEYCLOAK_BIND_ADDRESS=127.0.0.1 # 仅本机监听
KEYCLOAK_HTTP_PORT=19091 # 与宿主机端口映射一致
KEYCLOAK_REALM=tenant-realm # 租户 realm
'
reset_curl
assert_success "$test_root/inline-comment.log" '行内注释'
assert_probe_url 'http://127.0.0.1:19091/realms/tenant-realm/.well-known/openid-configuration' '行内注释归一化'
if grep -Fq '# 仅本机监听' "$curl_log"; then
  fail "行内注释没有被剥离：$(cat "$curl_log")"
fi

# 通配地址带引号/注释时同样必须归一化为回环。
write_runtime_config 'KEYCLOAK_BIND_ADDRESS="0.0.0.0" # 对外发布
KEYCLOAK_HTTP_PORT=18090
KEYCLOAK_REALM=basic-platform
'
reset_curl
assert_success "$test_root/quoted-wildcard.log" '带注释的引号通配地址'
assert_probe_url 'http://127.0.0.1:18090/realms/basic-platform/.well-known/openid-configuration' '带注释的引号通配归一化'

# 未加方括号的 IPv6 字面量必须补上方括号，否则 URL 无法解析。
write_runtime_config 'KEYCLOAK_BIND_ADDRESS=::1
KEYCLOAK_HTTP_PORT=18090
KEYCLOAK_REALM=basic-platform
'
reset_curl
assert_success "$test_root/ipv6.log" '未加方括号的 IPv6'
assert_probe_url 'http://[::1]:18090/realms/basic-platform/.well-known/openid-configuration' 'IPv6 加方括号'
if grep -Fq 'http://::1:' "$curl_log"; then
  fail "IPv6 字面量没有加方括号：$(cat "$curl_log")"
fi

# (e) compose 模型优先：模型里已解析出发布映射（含通配 host_ip）时，必须用它而不是 .env。
write_runtime_config 'KEYCLOAK_BIND_ADDRESS=127.0.0.1
KEYCLOAK_HTTP_PORT=18090
KEYCLOAK_REALM=basic-platform
'
compose_model='{"services":{"keycloak":{"image":"quay.io/keycloak/keycloak:26.2","ports":[{"mode":"ingress","host_ip":"127.0.0.1","target":8080,"published":"19999","protocol":"tcp"}]}}}'
reset_curl
assert_success "$test_root/compose-port.log" 'compose 模型端口优先'
assert_probe_url 'http://127.0.0.1:19999/realms/basic-platform/.well-known/openid-configuration' 'compose 模型发布的端口优先于 .env'
if grep -Fq 'http://127.0.0.1:18090/realms' "$curl_log"; then
  fail "compose 模型的发布端口没有优先于 .env：$(cat "$curl_log")"
fi

# 模型里 host_ip 是通配或空（宿主机所有地址）时，客户端仍然要换成回环地址。
compose_model='{"services":{"keycloak":{"ports":[{"host_ip":"0.0.0.0","target":8080,"published":"19999"}]}}}'
reset_curl
assert_success "$test_root/compose-wildcard.log" 'compose 模型通配 host_ip'
assert_probe_url 'http://127.0.0.1:19999/realms/basic-platform/.well-known/openid-configuration' 'compose 通配 host_ip 归一化'

compose_model='{"services":{"keycloak":{"ports":[{"target":8080,"published":"19999"}]}}}'
reset_curl
assert_success "$test_root/compose-empty-host.log" 'compose 模型缺省 host_ip'
assert_probe_url 'http://127.0.0.1:19999/realms/basic-platform/.well-known/openid-configuration' 'compose 缺省 host_ip 归一化'

# 模型里没有 keycloak 的 8080 映射（或模型不可读）=> 回退到归一化后的 .env。
compose_model='{"services":{"keycloak":{"ports":[{"host_ip":"127.0.0.1","target":9000,"published":"19000"}]}}}'
write_runtime_config 'KEYCLOAK_BIND_ADDRESS="0.0.0.0" # 对外发布
KEYCLOAK_HTTP_PORT=18090
KEYCLOAK_REALM=basic-platform
'
reset_curl
assert_success "$test_root/compose-missing-mapping.log" 'compose 无 8080 映射时回退 .env'
assert_probe_url 'http://127.0.0.1:18090/realms/basic-platform/.well-known/openid-configuration' '回退 .env 归一化'

compose_model='not-a-json-model'
reset_curl
assert_success "$test_root/compose-corrupt.log" 'compose 模型不可解析时回退 .env'
assert_probe_url 'http://127.0.0.1:18090/realms/basic-platform/.well-known/openid-configuration' '模型不可解析时回退 .env'
compose_model=''

# (f) Y3：master 200 + tenant 404（realm 尚未由 platform-worker bootstrap 创建）=> 警告但继续。
write_runtime_config 'KEYCLOAK_BIND_ADDRESS=127.0.0.1
KEYCLOAK_HTTP_PORT=18090
KEYCLOAK_REALM=basic-platform
'
reset_curl
tenant_code=404
tenant_body='{"error":"Realm does not exist"}'
assert_success "$test_root/realm-404.log" 'tenant realm 尚未创建'
tenant_url='http://127.0.0.1:18090/realms/basic-platform/.well-known/openid-configuration'
grep -Fq '404' "$test_root/realm-404.log" || fail 'tenant realm 404 没有出现在警告里'
grep -Fq 'basic-platform' "$test_root/realm-404.log" || fail 'tenant realm 404 警告没有点名具体 realm'
grep -Fq 'platform-worker' "$test_root/realm-404.log" ||
  fail 'tenant realm 404 警告没有点出 platform-worker 启动期 bootstrap'
grep -Fq 'EnsureKeycloakBroker' "$test_root/realm-404.log" ||
  fail 'tenant realm 404 警告没有点出 bootstrap 的具体入口'
grep -Fq 'KEYCLOAK_MANAGEMENT_ENABLED' "$test_root/realm-404.log" ||
  fail 'tenant realm 404 警告没有点出 KEYCLOAK_MANAGEMENT_ENABLED 这一支持配置'
if grep -Fq '卡死' "$test_root/realm-404.log"; then
  fail 'tenant realm 404 被误诊为 Keycloak 卡死'
fi
(( $(grep -Fc -- "$tenant_url" "$curl_log") == 6 )) ||
  fail "tenant realm 404 的有界重试次数不是 6 次：$(grep -Fc -- "$tenant_url" "$curl_log")"
# master 探针只发一次，不能被重试放大。
(( $(grep -Fc 'realms/master/.well-known' "$curl_log") == 1 )) ||
  fail 'master realm 探针被重复执行，重试只应作用于 tenant realm'

# tenant 404 之后恢复 200（platform-worker bootstrap 完成）=> 立即通过，不再重试。
reset_curl
tenant_code=200
assert_success "$test_root/realm-recovered.log" 'tenant realm 恢复可用'
(( $(grep -Fc -- "$tenant_url" "$curl_log") == 1 )) ||
  fail 'tenant realm 已经是 200 却仍然重试'

# (g) master realm 失败 = 真卡死：必须 fail-closed，并给出 2026-11-05 事故同型诊断。
reset_curl
master_status=22
master_code=000
master_body=''
assert_wedged_failure "$test_root/master-failure.log" 'master realm 连接失败' \
  'http://127.0.0.1:18090/realms/master/.well-known/openid-configuration'

reset_curl
master_code=503
master_body=''
assert_wedged_failure "$test_root/master-503.log" 'master realm 返回 5xx' \
  'http://127.0.0.1:18090/realms/master/.well-known/openid-configuration'

# 200 但响应体没有 issuer => 不是可用的 OIDC 元数据，同样必须失败。
reset_curl
master_body='{"realm":"master"}'
if require_keycloak_http_ready >"$test_root/missing-issuer.log" 2>&1; then
  fail '不含 issuer 的响应体被判为就绪：无法区分 OIDC 元数据与任意 200 响应'
fi
grep -Fq '缺少 issuer 字段' "$test_root/missing-issuer.log" ||
  fail '缺少“发现文档缺 issuer”的中文诊断'

# (h) master 200 但 tenant realm 5xx/连接失败 => 不是“realm 未创建”，仍是 fail-closed 卡死诊断。
reset_curl
tenant_code=500
tenant_body=''
assert_wedged_failure "$test_root/tenant-500.log" 'tenant realm 返回 5xx' \
  'http://127.0.0.1:18090/realms/basic-platform/.well-known/openid-configuration'

reset_curl
tenant_status=28
tenant_code=''
tenant_body=''
assert_wedged_failure "$test_root/tenant-timeout.log" 'tenant realm 超时' \
  'http://127.0.0.1:18090/realms/basic-platform/.well-known/openid-configuration'

# (i) 运行配置缺失时 fail-closed，不能靠内置默认值静默通过（否则门禁形同未接线）。
reset_curl
if (
  mv "$test_root/deploy/.env" "$test_root/deploy/.env.hidden"
  status=0
  require_keycloak_http_ready || status=$?
  mv "$test_root/deploy/.env.hidden" "$test_root/deploy/.env"
  exit "$status"
) >"$test_root/no-config.log" 2>&1; then
  fail '缺少运行配置时门禁仍然通过：缺口会被静默放行'
fi
grep -Fq '无法确定 Keycloak 探针地址' "$test_root/no-config.log" ||
  fail '缺少“无法确定探针地址”的中文诊断'
[[ ! -s "$curl_log" ]] || fail '缺少运行配置时仍发起了 curl 探测'

printf 'Keycloak HTTP 就绪门禁回归通过（回环默认值/通配归一化/自定义端口与 realm/引号与行内注释与空白/IPv6/compose 模型优先/realm 404 警告/master 卡死诊断/缺 issuer/缺配置/无公开入口）\n'
