#!/usr/bin/env bash
# 构建工作站专用：从工程根受控源码构建客户侧静态工具，不读取签名密钥或运行状态。
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
workspace_root="$(cd -- "$script_dir/../../../.." && pwd)"
output="${1:-}"
target_platform="${2:-linux/amd64}"
[[ $# -ge 1 && $# -le 2 && "$output" == /* ]] || {
  echo '用法：build-clock-recovery.sh /绝对路径/clock-recovery [linux/amd64]' >&2
  exit 2
}
[[ "$target_platform" == linux/amd64 ]] || {
  echo "clock-recovery 不支持此离线交付目标：$target_platform" >&2
  exit 2
}
[[ ! -e "$output" && ! -L "$output" && -d "$(dirname -- "$output")" ]] || {
  echo 'clock-recovery 输出必须是现有目录中的新文件' >&2
  exit 1
}
for required in commercial-license-tool/go.mod commercial-license-tool/cmd/clock-recovery/main.go \
  commercial-license-tool/internal/clockrecovery/recovery.go license-core/go.mod \
  license-core/license.go license-core/runtime/state.go; do
  [[ -f "$workspace_root/$required" && ! -L "$workspace_root/$required" ]] || {
    echo "clock-recovery 构建源码缺失或不是安全普通文件：$required" >&2
    exit 1
  }
done
for source_directory in commercial-license-tool commercial-license-tool/cmd \
  commercial-license-tool/cmd/clock-recovery commercial-license-tool/internal \
  commercial-license-tool/internal/clockrecovery license-core license-core/runtime; do
  [[ -d "$workspace_root/$source_directory" && ! -L "$workspace_root/$source_directory" ]] || {
    echo "clock-recovery 源码目录不能缺失或为符号链接：$source_directory" >&2
    exit 1
  }
done
for source_directory in commercial-license-tool/cmd/clock-recovery \
  commercial-license-tool/internal/clockrecovery license-core; do
  [[ -z "$(find "$workspace_root/$source_directory" -type l \
    \( -name '*.go' -o -name go.mod -o -name go.sum \) -print -quit)" ]] || {
    echo "clock-recovery 源码不能包含未纳入指纹的符号链接：$source_directory" >&2
    exit 1
  }
done
for command_name in go python3; do
  command -v "$command_name" >/dev/null || {
    echo "clock-recovery 构建工作站缺少命令：$command_name" >&2
    exit 1
  }
done

temporary="$(mktemp "$(dirname -- "$output")/.clock-recovery.build.XXXXXX")"
module_info="$(mktemp "$(dirname -- "$output")/.clock-recovery.module.XXXXXX")"
trap 'rm -f -- "$temporary" "$module_info"' EXIT

# 用途：固定交叉构建环境，避免工作区覆盖或全局 Go 配置改变受控输入。
# 参数：传递给 Go 的子命令和参数。
# 返回值：Go 命令的原始退出码；工具链或依赖缺失时失败。
build_go() (
  cd -- "$workspace_root/commercial-license-tool"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOENV=off GOFLAGS= \
    GOWORK=off GOTOOLCHAIN=local \
    GOPROXY="${OFFLINE_GOPROXY:-https://goproxy.cn|https://proxy.golang.org|direct}" \
    GOSUMDB="${OFFLINE_GOSUMDB:-sum.golang.google.cn}" go "$@"
)

build_go list -mod=readonly -m -json github.com/J-S-Te/license-core > "$module_info" || {
  echo 'clock-recovery 无法解析受控 license-core 依赖' >&2
  exit 1
}
python3 - "$module_info" "$workspace_root/license-core" <<'PY'
import json
from pathlib import Path
import sys

module = json.loads(Path(sys.argv[1]).read_text())
expected = Path(sys.argv[2])
replacement = Path(module.get("Replace", {}).get("Dir", ""))
if expected.is_symlink() or replacement.resolve() != expected.resolve():
    raise SystemExit("clock-recovery 必须使用工程根受控 license-core 依赖")
PY
build_go build -mod=readonly -trimpath -buildvcs=false \
  -ldflags='-s -w -buildid=' -o "$temporary" ./cmd/clock-recovery || {
  echo 'clock-recovery 静态构建失败；拒绝发布部署资产' >&2
  exit 1
}
python3 - "$temporary" <<'PY'
from pathlib import Path
import struct
import sys

raw = Path(sys.argv[1]).read_bytes()
if len(raw) < 64 or raw[:6] != b"\x7fELF\x02\x01" or struct.unpack_from("<H", raw, 18)[0] != 62:
    raise SystemExit("clock-recovery 产物不是 Linux/amd64 ELF")
offset = struct.unpack_from("<Q", raw, 32)[0]
width, count = struct.unpack_from("<HH", raw, 54)
if width < 56 or count == 0 or offset + width * count > len(raw):
    raise SystemExit("clock-recovery ELF 程序头无效")
if any(struct.unpack_from("<I", raw, offset + width * i)[0] in (2, 3) for i in range(count)):
    raise SystemExit("clock-recovery 产物依赖动态链接，拒绝交付")
PY
chmod 750 "$temporary"
mv -- "$temporary" "$output"
printf '已构建静态时钟恢复工具：%s\n' "$output"
