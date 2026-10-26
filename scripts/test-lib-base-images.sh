#!/usr/bin/env bash
# scripts/lib/base-images.sh 加载测试：source 真实脚本库后断言基础镜像收集、去重与拉取边界。
# 只读替身：假 docker 桩拦截全部 docker 调用，不触网、不实际拉取；临时 HOME 隔离本机配置。
set -euo pipefail

scripts_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
lib_file="${scripts_dir}/lib/base-images.sh"

[[ -f "$lib_file" ]] || { echo "缺少库文件：$lib_file" >&2; exit 1; }

test_root="$(mktemp -d "${TMPDIR:-/tmp}/docker-local-lib-base-images.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM
mkdir -p "${test_root}/bin" "${test_root}/home"

# 假 docker 桩：image inspect 一律报已存在；任何 pull 都会记录到日志，供断言判定未发生。
cat > "${test_root}/bin/docker" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "${DOCKER_STUB_LOG:?}"
case "$1" in
    image) exit 0 ;;
    pull) exit 0 ;;
    *) exit 0 ;;
esac
EOF
chmod +x "${test_root}/bin/docker"

harness="${test_root}/harness.sh"
cat > "$harness" <<EOF
set -Eeuo pipefail
log() { printf '[docker-local] %s\n' "\$*"; }
fail() { printf '[docker-local] ERROR: %s\n' "\$*" >&2; exit 1; }
sleep() { :; }
workspace_root="${scripts_dir}/.."            # 指向 platform 仓库，收集仓库内 Dockerfile 的基础镜像
source "${lib_file}"
EOF

assert_contains() {
    grep -Fq "$2" <<<"$1" || { echo "断言失败：输出缺少 $2
实际输出：
$1" >&2; exit 1; }
}

# 1. collect_dockerfile_base_images 在仓库上收集到 golang 与 alpine 基础镜像并去重。
collected="$(HOME="${test_root}/home" PATH="${test_root}/bin:/usr/bin:/bin" bash -c "$(cat "$harness")
collect_dockerfile_base_images
")"
assert_contains "$collected" "golang:"
assert_contains "$collected" "alpine:"
duplicates="$(sort <<<"$collected" | uniq -d)"
[[ -z "$duplicates" ]] || { echo "断言失败：收集结果未去重：
$duplicates" >&2; exit 1; }
echo "收集断言通过：$(wc -l <<<"$collected" | tr -d ' ') 个去重基础镜像（含 golang: 与 alpine:）"

# 2. prepare_base_images：镜像桩全部报已存在时只记录检查、绝不触发 pull。
stub_log="${test_root}/docker-stub.log"
: > "$stub_log"
prepare_out="$(HOME="${test_root}/home" PATH="${test_root}/bin:/usr/bin:/bin" DOCKER_STUB_LOG="$stub_log" bash -c "$(cat "$harness")
prepare_base_images
")"
if grep -q "^pull " "$stub_log"; then
    echo "断言失败：镜像已存在时仍执行了 docker pull：
$(grep '^pull ' "$stub_log")" >&2
    exit 1
fi
assert_contains "$prepare_out" "检查并串行准备基础镜像"
echo "拉取边界断言通过：镜像已存在时不触发 docker pull"

# 3. pull_image_with_retry：重试耗尽后必须以非零退出传播失败，而不是静默成功。
if HOME="${test_root}/home" PATH="${test_root}/bin:/usr/bin:/bin" bash -c "$(cat "$harness")
docker() { case "\$1" in image) return 1 ;; pull) return 1 ;; *) return 1 ;; esac; }
pull_image_with_retry golang:missing-test 2
" 2>"${test_root}/retry.err"; then
    echo "断言失败：拉取重试耗尽后未传播失败" >&2
    exit 1
fi
grep -q "基础镜像拉取失败：golang:missing-test" "${test_root}/retry.err" || {
    echo "断言失败：失败信息缺失：
$(cat "${test_root}/retry.err")" >&2
    exit 1
}
echo "失败传播断言通过：重试耗尽后以非零退出并给出恢复提示"

echo "test-lib-base-images.sh 全部通过"
