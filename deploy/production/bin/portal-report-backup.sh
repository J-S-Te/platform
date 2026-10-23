#!/usr/bin/env bash
# 由 backup-all 在门户数据库快照后加载。报告对象不可变，报告 worker 永不删除，因此后续文件快照包含该数据库快照引用的全部对象。
backup_portal_reports() {
  local project="$1" release="$2" destination="$3" volumes volume image
  volumes="$(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter 'label=com.docker.compose.volume=portal-reports-data')" || return 1
  [[ -n "$volumes" ]] || return 0
  [[ "$volumes" != *$'\n'* ]] || { echo '多个门户报告存储卷，拒绝猜测备份范围' >&2; return 1; }
  volume="$volumes"
  [[ "$volume" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || return 1
  image="$(awk -F= '$1=="CUSTOMER_PORTAL_IMAGE" {sub(/^[^=]*=/, ""); print; exit}' "$release")"
  [[ "$image" =~ @sha256:[a-f0-9]{64}$ ]] || { echo '门户报告存储存在但缺少不可变门户镜像，拒绝遗漏报告备份' >&2; return 1; }
  # 此只读辅助容器不提供凭据或网络。
  docker run --rm --pull never --network none --read-only --cap-drop ALL \
    --mount "type=volume,source=$volume,target=/reports,readonly" \
    --entrypoint /bin/sh "$image" -ec \
    'if find /reports -xdev \( -type l -o \( ! -type d ! -type f \) \) -print -quit | grep -q .; then exit 1; fi; tar -czf - -C /reports .' \
    > "$destination/portal-reports.tar.gz" || return 1
  gzip -t "$destination/portal-reports.tar.gz"
}
