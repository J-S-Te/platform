#!/usr/bin/env bash
# 会话治理（平台 RP 客户端）五份副本的哈希清单校验与同步。
#
# 背景：platform 本体没有 RP 客户端，但五个独立仓库各有一份内容完全相同的
# sessiongovernance 副本（client.go / rotation.go / client_test.go）。
# 没有校验时任何单边修改都会让副本静默分叉，直到某个仓库的会话续期行为与
# 平台不一致才被发现。本脚本以 contract_management 为唯一权威源，
# 用 scripts/session-governance.sha256 固定五份副本的 sha256。
#
# 用法：
#   bash scripts/check-session-governance-sync.sh [--check|--sync]
#   --check （默认）只比对，不一致时列出仓库与文件并 exit 1。
#   --sync  从 contract_management 覆盖其余四份，逐条打印 diff 摘要后重写清单。
#
# 兼容性：macOS 自带 bash 3.2（不使用关联数组等 4.x 语法）。
# 自测：可用 SESSION_GOVERNANCE_WORKSPACE_ROOT 指向一份工作树副本，
# 在不改动真实仓库的前提下验证差异检测与同步行为。
set -eu

root=${SESSION_GOVERNANCE_WORKSPACE_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)}
root=$(CDPATH= cd -- "$root" && pwd)
manifest="$root/scripts/session-governance.sha256"
authority=contract_management
repos='contract_management customer_and_opportunity project_management data_analysis Settlement'
files='client.go rotation.go client_test.go'

# dir_for 输出仓库内副本目录（相对仓库根；customer_and_opportunity 在 internal/shared 下）。
dir_for() {
  case "$1" in
    customer_and_opportunity) printf 'internal/shared/sessiongovernance' ;;
    *) printf 'internal/sessiongovernance' ;;
  esac
}

# entry_for 输出 <仓库>/<副本目录>/<文件>，相对工作区根，也是清单第二列。
entry_for() {
  printf '%s/%s/%s' "$1" "$(dir_for "$1")" "$2"
}

digest() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d ' ' -f 1
  else shasum -a 256 "$1" | cut -d ' ' -f 1; fi
}

# entries 输出清单应有的全部条目（排序后写入清单）。
entries() {
  for repo in $repos; do
    for file in $files; do
      printf '%s\n' "$(entry_for "$repo" "$file")"
    done
  done
}

# verify_paths 拒绝符号链接与缺失路径：副本必须是普通文件，
# 否则哈希校验会被链接目标绕过，同步也可能写到仓库外。
verify_paths() {
  for repo in $repos; do
    dir="$root/$repo/$(dir_for "$repo")"
    [ -d "$dir" ] || { echo "session governance copy directory missing: $repo/$(dir_for "$repo") (expected files: $files)" >&2; exit 1; }
    links=$(find "$dir" -type l -print)
    [ -z "$links" ] || { echo "session governance symlink rejected: $(printf '%s\n' "$links" | sed "s|^$root/||")" >&2; exit 1; }
    for file in $files; do
      [ -f "$dir/$file" ] || { echo "session governance file missing: $repo/$(dir_for "$repo")/$file" >&2; exit 1; }
      [ ! -L "$dir/$file" ] || { echo "session governance symlink rejected: $repo/$(dir_for "$repo")/$file" >&2; exit 1; }
    done
  done
}

# verify 比对清单与被校验副本；差异全部列出后再 exit 1，便于一次看全。
verify() {
  [ -f "$manifest" ] || { echo "session governance manifest missing: scripts/session-governance.sha256 (run --sync to create)" >&2; exit 1; }
  verify_paths
  expected=$(entries | sort)
  listed=$(awk 'NF==2 {print $2}' "$manifest" | sort)
  [ "$listed" = "$expected" ] || {
    echo "session governance manifest entry list mismatch; expected:" >&2
    printf '%s\n' "$expected" >&2
    echo "listed:" >&2
    printf '%s\n' "$listed" >&2
    exit 1
  }
  drift=0
  for entry in $expected; do
    hash=$(awk -v path="$entry" 'NF==2 && $2==path {print $1}' "$manifest")
    [ -n "$hash" ] || { echo "session governance manifest entry missing: $entry" >&2; drift=1; continue; }
    actual=$(digest "$root/$entry")
    [ "$actual" = "$hash" ] || { echo "session governance drift: $entry (run --sync to restore from $authority)" >&2; drift=1; }
  done
  [ "$drift" = 0 ] || exit 1
  printf 'session governance copies in sync: %s file(s) verified\n' "$(printf '%s\n' $expected | wc -l | tr -d ' ')"
}

# sync 从权威源覆盖其余副本；每处改动都打印 diff 摘要，禁止静默覆盖。
sync() {
  verify_paths
  changed=0
  for repo in $repos; do
    [ "$repo" = "$authority" ] && continue
    for file in $files; do
      source_rel=$(entry_for "$authority" "$file")
      target_rel=$(entry_for "$repo" "$file")
      if cmp -s "$root/$source_rel" "$root/$target_rel"; then
        printf 'unchanged: %s\n' "$target_rel"
        continue
      fi
      printf 'syncing: %s (authoritative: %s)\n' "$target_rel" "$source_rel"
      # 差异摘要只打印前 40 行，避免整文件刷屏；随后才覆盖，禁止静默。
      diff -u "$root/$target_rel" "$root/$source_rel" | sed -n '1,40p' || true
      cp "$root/$source_rel" "$root/$target_rel"
      changed=$((changed + 1))
    done
  done
  : >"$manifest"
  for entry in $(entries | sort); do
    printf '%s  %s\n' "$(digest "$root/$entry")" "$entry" >>"$manifest"
  done
  printf 'session governance manifest rewritten: scripts/session-governance.sha256 (%d file(s) updated)\n' "$changed"
  verify
}

mode=${1:---check}
case "$mode" in
  --check) verify ;;
  --sync) sync ;;
  *) echo 'usage: bash scripts/check-session-governance-sync.sh [--check|--sync]' >&2; exit 2 ;;
esac
