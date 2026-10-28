#!/usr/bin/env bash
set -Eeuo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/portal-report-backup.sh"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/uip-portal-backup.XXXXXX")"
trap 'rm -rf -- "$fixture"' EXIT
mkdir "$fixture/source" "$fixture/output" "$fixture/skip-output" "$fixture/reject-output" \
  "$fixture/empty-volume" "$fixture/data-volume"
printf 'encrypted-report-fixture' > "$fixture/source/owned.report"
printf 'encrypted-report-fixture' > "$fixture/data-volume/owned.report"
printf 'CUSTOMER_PORTAL_IMAGE=local/portal@sha256:%064d\n' 1 > "$fixture/release.env"
printf 'CUSTOMER_PORTAL_IMAGE=customer-opportunity/portal:pending\n' > "$fixture/release-placeholder.env"

fail() { echo "FAIL: $*" >&2; exit 1; }
contains() { grep -Fq -- "$2" "$1" || fail "$3"; }

volume_output=qa_portal-reports-data
volume_driver=local
volume_mountpoint="$fixture/empty-volume"
volume_inspect_failed=false
volume_ls_failed=false
portal_container_service=''
ps_failed=false
docker() {
  case "$1" in
    volume)
      case "${2:-}" in
        ls)
          [[ "$volume_ls_failed" != true ]] || return 9
          printf '%s' "$volume_output"
          ;;
        inspect)
          [[ "$volume_inspect_failed" != true ]] || return 9
          printf '%s|%s\n' "$volume_driver" "$volume_mountpoint"
          ;;
        *) return 94 ;;
      esac
      ;;
    ps)
      [[ "$ps_failed" != true ]] || return 9
      service=''
      for argument in "$@"; do
        case "$argument" in
          label=com.docker.compose.service=*) service="${argument#label=com.docker.compose.service=}" ;;
        esac
      done
      if [[ -n "$portal_container_service" && "$service" == "$portal_container_service" ]]; then
        printf 'portal-container-id'
      fi
      ;;
    run)
      [[ "$*" == *'--network none --read-only --cap-drop ALL'* ]] || return 91
      [[ "$*" == *'type=volume,source=qa_portal-reports-data,target=/reports,readonly'* ]] || return 92
      [[ "$*" != *'-e '* && "$*" != *'--env'* ]] || return 93
      tar -cf - -C "$fixture/source" . | gzip -c
      ;;
    *) return 90 ;;
  esac
}

expect_rejected() {
  local label="$1" stdout_file="$2" stderr_file="$3" release="$4"
  if backup_portal_reports qa "$release" "$fixture/reject-output" >"$stdout_file" 2>"$stderr_file"; then
    fail "portal report backup accepted $label"
  fi
  [[ ! -e "$fixture/reject-output/portal-reports.tar.gz" ]] || fail "rejected portal report backup left an archive: $label"
  contains "$stderr_file" '门户报告存储存在但缺少不可变门户镜像，拒绝遗漏报告备份' \
    "portal report backup did not keep the fail-closed message for $label"
}

# 不可变镜像：只读快照路径保持不变。
backup_portal_reports qa "$fixture/release.env" "$fixture/output"
gzip -t "$fixture/output/portal-reports.tar.gz"
tar -xzf "$fixture/output/portal-reports.tar.gz" -C "$fixture/output"
cmp "$fixture/source/owned.report" "$fixture/output/owned.report"

# 多个报告存储卷仍拒绝猜测备份范围。
volume_output=$'qa_one\nqa_two'
if backup_portal_reports qa "$fixture/release.env" "$fixture/output" 2>/dev/null; then
  fail 'portal report backup accepted multiple report volumes'
fi
volume_output=qa_portal-reports-data

# 无法枚举报告卷时拒绝继续。
volume_ls_failed=true
if backup_portal_reports qa "$fixture/release.env" "$fixture/output" 2>/dev/null; then
  fail 'portal report backup accepted a failed volume enumeration'
fi
volume_ls_failed=false

# 占位镜像 + 空卷 + 无门户容器：全新首装明确跳过，且不产生报告归档。
if ! backup_portal_reports qa "$fixture/release-placeholder.env" "$fixture/skip-output" \
    >"$fixture/skip.stdout" 2>"$fixture/skip.stderr"; then
  fail 'fresh install with an empty report volume and no portal container was rejected'
fi
[[ ! -e "$fixture/skip-output/portal-reports.tar.gz" ]] || fail 'fresh install skip still produced a portal report archive'
contains "$fixture/skip.stdout" '全新首装跳过门户报告备份' 'fresh install skip explanation is missing'
if grep -Fq '缺少不可变门户镜像' "$fixture/skip.stderr"; then
  fail 'fresh install skip still emitted the fail-closed message'
fi

# 空卷但任一门户容器存在：仍 fail-closed。
for portal_service in portal-api portal-report-worker portal-invite-compensation-worker portal-migrate; do
  portal_container_service="$portal_service"
  expect_rejected "a $portal_service container with a placeholder image" \
    "$fixture/container-$portal_service.stdout" "$fixture/container-$portal_service.stderr" \
    "$fixture/release-placeholder.env"
done
portal_container_service=''

# 非空报告卷 + 占位镜像：仍 fail-closed。
volume_mountpoint="$fixture/data-volume"
expect_rejected 'a data-bearing report volume with a placeholder image' \
  "$fixture/data-volume.stdout" "$fixture/data-volume.stderr" "$fixture/release-placeholder.env"
volume_mountpoint="$fixture/empty-volume"

# 非 local driver 的报告卷即使目录为空也按已有数据处理。
volume_driver=nfs
expect_rejected 'a non-local report volume with a placeholder image' \
  "$fixture/foreign-volume.stdout" "$fixture/foreign-volume.stderr" "$fixture/release-placeholder.env"
volume_driver=local

# 无法核验卷内容或门户容器时仍 fail-closed。
volume_inspect_failed=true
expect_rejected 'an unverifiable report volume with a placeholder image' \
  "$fixture/inspect-failed.stdout" "$fixture/inspect-failed.stderr" "$fixture/release-placeholder.env"
volume_inspect_failed=false
ps_failed=true
expect_rejected 'unverifiable portal containers with a placeholder image' \
  "$fixture/ps-failed.stdout" "$fixture/ps-failed.stderr" "$fixture/release-placeholder.env"
ps_failed=false

# 报告卷不存在时保持原有早退语义（不变）。
volume_output=''
backup_portal_reports qa "$fixture/release.env" "$fixture/output"

echo 'Portal immutable vault backup, read-only isolation, fresh-install empty-volume skip and retained-volume rejection passed'
