#!/usr/bin/env bash
# 备份保留清理工具（默认 dry-run，只有显式 --apply 才删除）。
#
# 背景（2026-11-06 测试服务器真实事故）：5 个仓并发部署时每次都写发布前数据库备份，
# /opt/basic-platform/backups 被撑满，gzip 报 "No space left on device"，4 个仓发布被拒。
#
# 保留规则：一个备份“组”必须同时满足以下两点才删除 ——
#   1) 组的最新写入时间早于 --keep-days（默认 7 天）；
#   2) 该组不属于最近 --keep-count（默认 5）份完整备份组。
# “组”是删除的最小单位，绝不拆散文件与其 .sha256 / binlog-position sidecar：
#   - backups/<label>-<release>.sql.gz（+ .sha256 / sidecar）
#   - backups/system/<UTC stamp>-<pid>/（灾备批次目录，含 tar/sql/清单）
#   - backups/file-gateway/<UTC stamp>/（网关快照批次目录）
#   - backups/keycloak/keycloak-<UTC stamp>.sql.gz（+ sidecar）
# 运行配置快照（backups/releases/*.env、configuration-*、install-assets.*）与未知条目
# 一律跳过。符号链接、特殊文件、deploy-dir 之外的路径一律拒绝。
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"

usage() {
  cat >&2 <<'EOF'
usage: prune-backups.sh [--deploy-dir DIR] [--keep-days N] [--keep-count M] [--apply]

  --deploy-dir DIR   部署目录（默认：本脚本所在 bin/ 的父目录）；必须位于 backups 的上一层
  --keep-days N      保留天数，早于 N 天才可能删除（默认 7；0 表示只按份数保留）
  --keep-count M     至少保留最近 M 份完整备份组（默认 5；最小 1）
  --apply            真正删除；缺省只打印计划（dry-run）

Deletion requires BOTH conditions: a group older than keep-days AND not among the newest
keep-count complete backup groups. A group is deleted as a whole (including .sha256 and
binlog-position sidecars). Runtime configuration snapshots are never deleted.
EOF
  exit "${1:-64}"
}

deploy_dir=""
keep_days=7
keep_count=5
apply=false

while (($#)); do
  case "$1" in
    --deploy-dir) (($# >= 2)) || usage; deploy_dir="$2"; shift 2 ;;
    --keep-days) (($# >= 2)) || usage; keep_days="$2"; shift 2 ;;
    --keep-count) (($# >= 2)) || usage; keep_count="$2"; shift 2 ;;
    --apply) apply=true; shift ;;
    -h|--help) usage 0 ;;
    *) usage ;;
  esac
done

# 只接受非负十进制整数，避免 "7;rm"、空串或浮点数进入算术上下文。
[[ "$keep_days" =~ ^[0-9]+$ ]] || {
  echo "prune-backups: --keep-days must be a non-negative integer: $keep_days" >&2
  exit 64
}
[[ "$keep_count" =~ ^[0-9]+$ && "$keep_count" -ge 1 ]] || {
  echo "prune-backups: --keep-count must be an integer >= 1: $keep_count" >&2
  exit 64
}

if [[ -z "$deploy_dir" ]]; then
  deploy_dir="$(cd -P -- "$script_dir/.." && pwd -P)"
fi
[[ "$deploy_dir" == /* ]] || {
  echo "prune-backups: --deploy-dir must be an absolute path: $deploy_dir" >&2
  exit 64
}
[[ -d "$deploy_dir" && ! -L "$deploy_dir" ]] || {
  echo "prune-backups: deploy dir is missing, not a directory, or a symbolic link: $deploy_dir" >&2
  exit 1
}
deploy_dir="$(cd -P -- "$deploy_dir" && pwd -P)"
backup_root="$deploy_dir/backups"

if [[ ! -e "$backup_root" && ! -L "$backup_root" ]]; then
  echo "backup prune: no backups directory ($backup_root); nothing to do"
  exit 0
fi
[[ -d "$backup_root" && ! -L "$backup_root" ]] || {
  echo "prune-backups: refusing to prune: backups root is not a regular directory: $backup_root" >&2
  exit 1
}
system_root="$backup_root/system"
keycloak_root="$backup_root/keycloak"
gateway_root="$backup_root/file-gateway"

# 兼容 GNU coreutils 与 BSD/macOS 的测试环境；生产服务器走 stat -c。
mtime_of() { stat -c '%Y' "$1" 2>/dev/null || stat -f '%m' "$1"; }
size_of() { stat -c '%s' "$1" 2>/dev/null || stat -f '%z' "$1"; }
is_regular() { [[ -f "$1" && ! -L "$1" ]]; }
has_special() { find "$1" \( -type l -o ! -type d ! -type f \) -print -quit 2>/dev/null | grep -q .; }

group_paths=()
add_group() { group_paths+=("$1"); }
skip_note() { printf 'SKIP %s (%s)\n' "$1" "$2"; }

# 顶层发布前数据库备份：<label>-<release>.sql.gz + .sha256。隐藏临时文件（mktemp 产物）不参与。
for entry in "$backup_root"/*; do
  [[ -e "$entry" || -L "$entry" ]] || continue
  name="${entry##*/}"
  case "$name" in
    *.sql.gz)
      if [[ "$name" == .* ]]; then
        continue
      fi
      if [[ "$name" == *[$'\t\n']* ]]; then
        skip_note "$entry" 'unsupported control character in file name'
        continue
      fi
      if [[ -L "$entry" ]]; then
        skip_note "$entry" 'symbolic link'
        continue
      fi
      [[ -f "$entry" ]] || { skip_note "$entry" 'not a regular file'; continue; }
      if ! is_regular "$entry.sha256"; then
        skip_note "$entry" 'incomplete backup group (missing regular .sha256)'
        continue
      fi
      add_group "$entry"
      ;;
    system|keycloak|file-gateway|releases) ;;
    .*) ;;
    *.sql.gz.sha256|*.sql.gz.binlog-position|*.sql.gz.binlog-position.sha256) ;;
    *)
      case "$name" in
        configuration-*|install-assets.*|*.env)
          skip_note "$entry" 'retained runtime/configuration snapshot'
          ;;
        *)
          skip_note "$entry" 'not a recognized backup artifact'
          ;;
      esac
      ;;
  esac
done

# backups/releases：release.env 快照，属运行配置，永远保留。
[[ ! -d "$backup_root/releases" ]] || skip_note "$backup_root/releases" 'retained runtime configuration (release env snapshots)'

# 灾备批次：backup-all.sh 的 <UTC stamp>-<pid>/ 目录；缺少 MANIFEST/SHA256SUMS 视为未完成，绝不删除。
if [[ -L "$system_root" ]]; then
  skip_note "$system_root" 'symbolic link'
elif [[ -e "$system_root" ]]; then
  [[ -d "$system_root" ]] || skip_note "$system_root" 'not a directory'
  if [[ -d "$system_root" ]]; then
    while IFS= read -r -d '' entry; do
      name="${entry##*/}"
      case "$name" in
        .*) skip_note "$entry" 'in-progress or hidden temporary'; continue ;;
      esac
      [[ "$name" =~ ^[0-9]{8}T[0-9]{6}Z(-[0-9]+)?$ ]] || {
        skip_note "$entry" 'unrecognized entry in backups/system'
        continue
      }
      if [[ -L "$entry" ]]; then
        skip_note "$entry" 'symbolic link'
        continue
      fi
      [[ -d "$entry" ]] || { skip_note "$entry" 'not a directory'; continue; }
      if ! is_regular "$entry/MANIFEST" || ! is_regular "$entry/SHA256SUMS"; then
        skip_note "$entry" 'incomplete disaster backup group (MANIFEST/SHA256SUMS)'
        continue
      fi
      if has_special "$entry"; then
        skip_note "$entry" 'contains symbolic link or special file'
        continue
      fi
      add_group "$entry"
    done < <(find "$system_root" -mindepth 1 -maxdepth 1 -print0)
  fi
fi

# Keycloak 备份：只有完整 sql.gz + 摘要才算一组；位点 sidecar 随组删除。
if [[ -L "$keycloak_root" ]]; then
  skip_note "$keycloak_root" 'symbolic link'
elif [[ -e "$keycloak_root" ]]; then
  [[ -d "$keycloak_root" ]] || skip_note "$keycloak_root" 'not a directory'
  if [[ -d "$keycloak_root" ]]; then
    while IFS= read -r -d '' entry; do
      name="${entry##*/}"
      case "$name" in
        .*) continue ;;
        keycloak-*.sql.gz.binlog-position|keycloak-*.sql.gz.binlog-position.sha256|keycloak-*.sql.gz.sha256)
          continue ;;
        keycloak-*.sql.gz)
          [[ "$name" =~ ^keycloak-[0-9]{8}T[0-9]{6}Z\.sql\.gz$ ]] || {
            skip_note "$entry" 'unrecognized Keycloak backup name'
            continue
          }
          if [[ -L "$entry" ]]; then
            skip_note "$entry" 'symbolic link'
            continue
          fi
          [[ -f "$entry" ]] || { skip_note "$entry" 'not a regular file'; continue; }
          if ! is_regular "$entry.sha256"; then
            skip_note "$entry" 'incomplete Keycloak backup group (missing regular .sha256)'
            continue
          fi
          add_group "$entry"
          ;;
        *)
          skip_note "$entry" 'not a recognized Keycloak backup artifact'
          ;;
      esac
    done < <(find "$keycloak_root" -mindepth 1 -maxdepth 1 -print0)
  fi
fi

# 独立 File Gateway 快照：<UTC stamp>/ 目录一组，要求 MANIFEST/SHA256SUMS。
if [[ -L "$gateway_root" ]]; then
  skip_note "$gateway_root" 'symbolic link'
elif [[ -e "$gateway_root" ]]; then
  [[ -d "$gateway_root" ]] || skip_note "$gateway_root" 'not a directory'
  if [[ -d "$gateway_root" ]]; then
    while IFS= read -r -d '' entry; do
      name="${entry##*/}"
      case "$name" in
        .*) skip_note "$entry" 'in-progress or hidden temporary'; continue ;;
      esac
      [[ "$name" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || {
        skip_note "$entry" 'unrecognized entry in backups/file-gateway'
        continue
      }
      if [[ -L "$entry" ]]; then
        skip_note "$entry" 'symbolic link'
        continue
      fi
      [[ -d "$entry" ]] || { skip_note "$entry" 'not a directory'; continue; }
      if ! is_regular "$entry/MANIFEST" || ! is_regular "$entry/SHA256SUMS"; then
        skip_note "$entry" 'incomplete File Gateway backup group (MANIFEST/SHA256SUMS)'
        continue
      fi
      if has_special "$entry"; then
        skip_note "$entry" 'contains symbolic link or special file'
        continue
      fi
      add_group "$entry"
    done < <(find "$gateway_root" -mindepth 1 -maxdepth 1 -print0)
  fi
fi

# 组内文件清单：目录组整体删除；文件组包含主文件及已存在的 sidecar。
group_files_of() {
  local group="$1" suffix
  if [[ -d "$group" ]]; then
    printf '%s\n' "$group"
    return 0
  fi
  printf '%s\n' "$group"
  for suffix in .sha256 .binlog-position .binlog-position.sha256; do
    if [[ -e "$group$suffix" || -L "$group$suffix" ]]; then
      printf '%s\n' "$group$suffix"
    fi
  done
}

group_is_safe() {
  local group="$1" path
  [[ "$group" == "$backup_root"/* ]] || return 1
  [[ "$group" != "$backup_root" && "$group" != "$backup_root/" ]] || return 1
  while IFS= read -r path; do
    [[ "$path" == "$backup_root"/* ]] || return 1
    if [[ -L "$path" ]]; then
      return 1
    fi
    [[ -f "$path" || -d "$path" ]] || return 1
  done < <(group_files_of "$group")
  if [[ -d "$group" ]] && has_special "$group"; then
    return 1
  fi
  return 0
}

group_kib() {
  local group="$1" path total=0 bytes
  if [[ -d "$group" ]]; then
    du -sk "$group" 2>/dev/null | awk 'NR == 1 { print $1 }'
    return 0
  fi
  while IFS= read -r path; do
    bytes="$(size_of "$path")"
    total=$((total + bytes))
  done < <(group_files_of "$group")
  printf '%s\n' $(((total + 1023) / 1024))
}

# df -P 避免长设备名换行；第四列是可用 KiB。取不到时返回空，不影响删除结论。
available_kib() { df -P -k "$backup_root" 2>/dev/null | awk 'NR == 2 { print $4 }'; }

printf 'backup prune: deploy-dir=%s\n' "$deploy_dir"
printf 'backup prune: backups=%s\n' "$backup_root"
printf 'backup prune: policy keep-days=%s keep-count=%s; deletion requires age > keep-days AND not in the newest keep-count groups\n' \
  "$keep_days" "$keep_count"
printf 'backup prune: candidate groups=%s\n' "${#group_paths[@]}"

# 按组的“最新写入时间”排序：目录组取目录 mtime，文件组取 sql.gz mtime。
# 注意：这里刻意不使用 mktemp/临时文件。磁盘已满时 mktemp 会失败并让清理工具本身
# 无法运行（2026-11-07 真实事故：磁盘 100% 时 prune 在 candidate groups 之后 exit 1）。
# 用进程替换把排序结果直接读入内存，清理工具必须能在“零可用字节”时仍然工作。
ranked=()
mtimes=()
while IFS=$'\t' read -r mtime group; do
  [[ -n "$group" ]] || continue
  ranked+=("$group")
  mtimes+=("$mtime")
done < <(
  group_index=0
  while ((group_index < ${#group_paths[@]})); do
    group="${group_paths[$group_index]}"
    printf '%s\t%s\n' "$(mtime_of "$group")" "$group"
    group_index=$((group_index + 1))
  done | LC_ALL=C sort -t $'\t' -k1,1nr -k2,2
)

now="$(date -u +%s)"
delete_groups=()
projected_kib=0
index=0
while ((index < ${#ranked[@]})); do
  group="${ranked[$index]}"
  mtime="${mtimes[$index]}"
  age_seconds=$((now - mtime))
  if ((age_seconds < 0)); then age_seconds=0; fi
  age_days=$((age_seconds / 86400))
  if ((index < keep_count)); then
    printf 'KEEP %s (recent #%d of %d; age %dd)\n' "$group" $((index + 1)) "$keep_count" "$age_days"
  elif ((age_seconds > keep_days * 86400)); then
    delete_groups+=("$group")
    kib="$(group_kib "$group")"
    projected_kib=$((projected_kib + kib))
    printf 'DELETE %s (age %dd; %s KiB)\n' "$group" "$age_days" "$kib"
  else
    printf 'KEEP %s (age %dd; newer than %d days)\n' "$group" "$age_days" "$keep_days"
  fi
  index=$((index + 1))
done

printf 'disk current:\n'
df -h "$backup_root" || true

if [[ "$apply" != true ]]; then
  printf 'backup prune: would delete %d group(s), would free %d KiB (dry-run; no files were deleted; re-run with --apply to delete)\n' \
    "${#delete_groups[@]}" "$projected_kib"
  exit 0
fi

printf 'disk before:\n'
df -h "$backup_root" || true
available_before="$(available_kib)"

freed_kib=0
deleted=0
failed=0
delete_index=0
while ((delete_index < ${#delete_groups[@]})); do
  group="${delete_groups[$delete_index]}"
  delete_index=$((delete_index + 1))
  if ! group_is_safe "$group"; then
    echo "WARNING: refusing to delete group outside backups or containing symbolic links: $group" >&2
    failed=$((failed + 1))
    continue
  fi
  kib="$(group_kib "$group")"
  status=0
  while IFS= read -r path; do
    if [[ -d "$path" && ! -L "$path" ]]; then
      rm -rf -- "$path" || status=1
    else
      rm -f -- "$path" || status=1
    fi
  done < <(group_files_of "$group")
  if ((status == 0)); then
    freed_kib=$((freed_kib + kib))
    deleted=$((deleted + 1))
    printf 'deleted %s (%d KiB)\n' "$group" "$kib"
  else
    echo "WARNING: failed to remove every file in backup group: $group" >&2
    failed=$((failed + 1))
  fi
done

printf 'disk after:\n'
df -h "$backup_root" || true
available_after="$(available_kib)"
printf 'backup prune: deleted %d group(s), freed %d KiB\n' "$deleted" "$freed_kib"
if [[ "$available_before" =~ ^[0-9]+$ && "$available_after" =~ ^[0-9]+$ ]]; then
  printf 'backup prune: df available before=%s KiB after=%s KiB delta=%s KiB\n' \
    "$available_before" "$available_after" $((available_after - available_before))
fi
if ((failed > 0)); then
  echo "backup prune: $failed group(s) could not be removed; inspect the warnings above" >&2
fi
exit 0
