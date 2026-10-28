#!/usr/bin/env bash
# 由 backup-all 在门户数据库快照后加载。报告对象不可变，报告 worker 永不删除，因此后续文件快照包含该数据库快照引用的全部对象。

# 全新首装时 compose 会预创建空报告卷，而业务包在 frontend 阶段之后才导入，
# 此时 .release.env 里的 CUSTOMER_PORTAL_IMAGE 可能仍是占位 tag。仅当卷从未写入
# 数据且项目内不存在任何门户容器时才允许跳过；任何无法核验的情况保持 fail-closed。
portal_report_volume_is_unused() {
  local project="$1" volume="$2" metadata driver mountpoint entry service container
  [[ -n "$volume" && "$volume" != *$'\n'* ]] || return 1
  metadata="$(docker volume inspect --format '{{.Driver}}|{{.Mountpoint}}' "$volume" 2>/dev/null)" || return 1
  driver="${metadata%%|*}"
  mountpoint="${metadata#*|}"
  [[ "$driver" == local ]] || return 1
  [[ "$mountpoint" == /* && -d "$mountpoint" && ! -L "$mountpoint" ]] || return 1
  entry="$(find "$mountpoint" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" || return 1
  [[ -z "$entry" ]] || return 1
  for service in portal-api portal-report-worker portal-invite-compensation-worker portal-migrate; do
    container="$(docker ps -aq \
      --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$service")" || return 1
    [[ -z "$container" ]] || return 1
  done
}

backup_portal_reports() {
  local project="$1" release="$2" destination="$3" volumes volume image
  volumes="$(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter 'label=com.docker.compose.volume=portal-reports-data')" || return 1
  [[ -n "$volumes" ]] || return 0
  [[ "$volumes" != *$'\n'* ]] || { echo '多个门户报告存储卷，拒绝猜测备份范围' >&2; return 1; }
  volume="$volumes"
  [[ "$volume" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || return 1
  image="$(awk -F= '$1=="CUSTOMER_PORTAL_IMAGE" {sub(/^[^=]*=/, ""); print; exit}' "$release")"
  if [[ ! "$image" =~ @sha256:[a-f0-9]{64}$ ]]; then
    if portal_report_volume_is_unused "$project" "$volume"; then
      echo '门户报告存储卷从未写入数据且项目内不存在门户容器；全新首装跳过门户报告备份'
      return 0
    fi
    echo '门户报告存储存在但缺少不可变门户镜像，拒绝遗漏报告备份' >&2
    return 1
  fi
  # 此只读辅助容器不提供凭据或网络。
  docker run --rm --pull never --network none --read-only --cap-drop ALL \
    --mount "type=volume,source=$volume,target=/reports,readonly" \
    --entrypoint /bin/sh "$image" -ec \
    'if find /reports -xdev \( -type l -o \( ! -type d ! -type f \) \) -print -quit | grep -q .; then exit 1; fi; tar -czf - -C /reports .' \
    > "$destination/portal-reports.tar.gz" || return 1
  gzip -t "$destination/portal-reports.tar.gz"
}
