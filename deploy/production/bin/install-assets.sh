#!/usr/bin/env bash
# Install a verified deployment-assets archive while retaining local module selection.
set -Eeuo pipefail

usage() {
  cat >&2 <<'EOF'
用法：
  install-assets.sh deployment-assets.tar.gz [部署目录]
  install-assets.sh --recover [部署目录]

--recover 会使用未完成事务创建的本地备份恢复安装前的部署资产；不会修改运行配置、
数据库卷或备份内容。恢复完成前 deploy.sh 会拒绝继续运行。

仅受控平台镜像发布可设置 ASSETS_PLATFORM_UPGRADE_IMAGE=<不可变 digest> 继续
未完成的资产升级；门禁会绑定该镜像，并保留到平台迁移和新 API/Agent 验证完成。
EOF
}

validate_target() {
  [[ "$target" == /* && "$target" != / ]] || { echo '部署目录必须为非根目录的绝对路径' >&2; exit 2; }
}

safe_top_name() {
  [[ "$1" =~ ^(bin|subsystems\.d|subsystem-templates|mysql-init|nginx|monitoring|tests|docker-compose\.yml|docker-compose\.yml\.dist|\.env\.example|\.release\.env\.example|\.gitignore|ACCEPTANCE_CHECKLIST\.md|README\.md|OFFLINE_DEPLOYMENT\.md|OFFLINE_RUNBOOK\.md|DEPLOYMENT_COMPATIBILITY\.md|BACKUP_RECOVERY\.md)$ ]]
}

compose_project_name() {
  local configured=''
  if [[ -f "$target/.env" && ! -L "$target/.env" ]]; then
    configured="$(awk -F= '
      $1 == "COMPOSE_PROJECT_NAME" {
        value=substr($0, index($0, "=") + 1)
      }
      END {print value}
    ' "$target/.env")"
  fi
  if [[ -n "$configured" ]]; then
    [[ "$configured" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]] || {
      echo "COMPOSE_PROJECT_NAME 格式不安全：$configured" >&2
      return 1
    }
    printf '%s\n' "$configured"
  else
    printf '%s\n' 'basic-platform-production'
  fi
}

control_plane_is_running() {
  local project service container
  [[ -f "$target/.env" && ! -L "$target/.env" ]] || return 1
  command -v docker >/dev/null || {
    echo '检测到已有运行配置，但缺少 docker，无法确认控制面是否正在运行' >&2
    return 2
  }
  docker info >/dev/null 2>&1 || {
    echo '检测到已有运行配置，但无法访问 Docker，拒绝在未知运行状态下替换部署资产' >&2
    return 2
  }
  project="$(compose_project_name)" || return 2
  for service in platform-api subsystem-provisioner; do
    container="$(docker ps -q \
      --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$service" 2>/dev/null | head -n 1 || true)"
    [[ -z "$container" ]] || return 0
  done
  return 1
}

write_control_plane_reload_marker() {
  local marker="$target/runtime/.control-plane-reload-required" temporary
  temporary="$(mktemp "$target/runtime/.control-plane-reload-required.prepare.XXXXXX")"
  {
    printf 'FORMAT=1\n'
    printf 'ASSETS_ARCHIVE_SHA256=%s\n' "$actual"
    printf 'INSTALLED_AT_UTC=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    printf 'REASON=deployment_assets_changed_while_control_plane_running\n'
    if [[ -n "${ASSETS_PLATFORM_UPGRADE_IMAGE:-}" ]]; then
      printf 'PLATFORM_UPGRADE_IMAGE=%s\n' "$ASSETS_PLATFORM_UPGRADE_IMAGE"
    fi
  } >"$temporary"
  chmod 600 "$temporary"
  mv -f -- "$temporary" "$marker"
}

recover_install() {
  validate_target
  [[ -d "$target" && ! -L "$target" ]] || { echo "部署目录不存在或不安全：$target" >&2; exit 1; }
  install -d -m 700 "$target/runtime" "$target/backups"
  exec 9>"$target/runtime/.deploy.lock"
  flock -w 900 9 || { echo '等待发布锁超时' >&2; exit 1; }
  transaction="$target/runtime/.assets-install-transaction"
  [[ -d "$transaction" && ! -L "$transaction" ]] || { echo "没有可恢复的部署资产安装事务：$transaction" >&2; exit 1; }
  [[ -f "$transaction/backup" && -f "$transaction/touched" && -f "$transaction/existing" ]] || {
    echo "安装事务元数据不完整，拒绝自动恢复：$transaction" >&2; exit 1;
  }
  backup="$(awk 'NR == 1 {value=$0; next} {bad=1} END {if (bad || NR != 1) exit 1; print value}' "$transaction/backup")" || {
    echo '安装事务备份路径格式无效' >&2; exit 1;
  }
  case "$backup" in "$target"/backups/install-assets.*) ;; *) echo "安装事务备份路径越界：$backup" >&2; exit 1 ;; esac
  [[ -d "$backup" && ! -L "$backup" ]] || { echo "安装事务备份不存在或不安全：$backup" >&2; exit 1; }

  while IFS= read -r name; do
    [[ -n "$name" ]] || continue
    safe_top_name "$name" || { echo "安装事务包含非法目标：$name" >&2; exit 1; }
    [[ ! -L "$target/$name" ]] || { echo "恢复目标被替换为符号链接，拒绝自动处理：$target/$name" >&2; exit 1; }
    rm -rf -- "$target/$name"
    if grep -Fxq -- "$name" "$transaction/existing"; then
      [[ -e "$backup/$name" && ! -L "$backup/$name" ]] || { echo "备份缺少原资产：$name" >&2; exit 1; }
      cp -a -- "$backup/$name" "$target/$name"
    fi
  done < "$transaction/touched"
  if [[ -e "$transaction/previous-reload-marker" || -L "$transaction/previous-reload-marker" ]]; then
    [[ -f "$transaction/previous-reload-marker" && ! -L "$transaction/previous-reload-marker" && ! -L "$target/runtime/.control-plane-reload-required" ]] || {
      echo '恢复事务中的重载标记不是安全普通文件' >&2; exit 1;
    }
    cp -p -- "$transaction/previous-reload-marker" "$target/runtime/.control-plane-reload-required"
  elif [[ -f "$target/runtime/.control-plane-reload-required" && -f "$transaction/archive-sha256" ]]; then
    transaction_digest="$(awk 'NR == 1 {print $1}' "$transaction/archive-sha256")"
    marker_digest="$(awk -F= '$1 == "ASSETS_ARCHIVE_SHA256" {print $2}' "$target/runtime/.control-plane-reload-required")"
    if [[ -n "$transaction_digest" && "$marker_digest" == "$transaction_digest" ]]; then
      rm -f -- "$target/runtime/.control-plane-reload-required"
    fi
  fi
  rm -rf -- "$transaction"
  echo "部署资产已恢复到安装前状态；保留备份供审计：$backup"
}

if [[ "${1:-}" == --recover ]]; then
  target="${2:-/opt/unified-identity-platform}"
  (($# <= 2)) || { usage; exit 2; }
  recover_install
  exit 0
fi

(($# >= 1 && $# <= 2)) || { usage; exit 2; }
archive="$1"
target="${2:-/opt/unified-identity-platform}"
validate_target
if [[ -n "${ASSETS_PLATFORM_UPGRADE_IMAGE:-}" && ! "$ASSETS_PLATFORM_UPGRADE_IMAGE" =~ ^[^[:space:]]+@sha256:[a-f0-9]{64}$ ]]; then
  echo '受控平台升级必须提供不可变镜像 digest' >&2
  exit 2
fi
archive="$(cd "$(dirname "$archive")" && pwd)/$(basename "$archive")"
[[ -f "$archive" && ! -L "$archive" && -f "$archive.sha256" && ! -L "$archive.sha256" ]] || {
  echo '缺少普通文件形式的资产包或 SHA256 伴随文件' >&2; exit 1;
}
expected="$(awk -v name="$(basename -- "$archive")" '
  NF != 2 || NR != 1 || length($1) != 64 || tolower($1) ~ /[^0-9a-f]/ || $2 != name {bad=1}
  {digest=tolower($1)}
  END {if (bad || NR != 1) exit 1; print digest}
' "$archive.sha256")" || { echo '资产包 SHA256 伴随文件必须只绑定当前包' >&2; exit 1; }
actual="$(sha256sum "$archive" | awk '{print tolower($1)}')"
[[ "$expected" == "$actual" ]] || { echo '资产包 SHA256 校验失败' >&2; exit 1; }
listing="$(tar -tzf "$archive")"
printf '%s\n' "$listing" | awk '
  { sub(/^\.\//, ""); if ($0 == "" || $0 == ".") next;
    if ($0 ~ /^\// || $0 ~ /(^|\/)\.\.($|\/)/) exit 1;
    split($0, a, "/");
    if (a[1] !~ /^(bin|subsystems\.d|subsystem-templates|mysql-init|nginx|monitoring|tests|docker-compose\.yml|\.env\.example|\.release\.env\.example|\.gitignore|ACCEPTANCE_CHECKLIST\.md|README\.md|OFFLINE_DEPLOYMENT\.md|OFFLINE_RUNBOOK\.md|DEPLOYMENT_COMPATIBILITY\.md|BACKUP_RECOVERY\.md)$/) exit 1;
  }' || { echo '资产包含非部署文件或不安全路径' >&2; exit 1; }
tar -tvzf "$archive" | awk 'substr($1,1,1) != "-" && substr($1,1,1) != "d" {exit 1}' || { echo '资产禁止包含链接或特殊文件' >&2; exit 1; }

install -d -m 750 "$target"
[[ ! -L "$target/runtime" && ! -L "$target/backups" ]] || { echo '运行或备份目录不能为符号链接' >&2; exit 1; }
install -d -m 700 "$target/runtime" "$target/backups"
exec 9>"$target/runtime/.deploy.lock"
flock -w 900 9 || { echo '等待发布锁超时' >&2; exit 1; }
transaction="$target/runtime/.assets-install-transaction"
[[ ! -e "$transaction" && ! -L "$transaction" ]] || {
  echo "检测到未完成的资产安装事务，拒绝叠加安装：$transaction" >&2
  echo "请先执行：$0 --recover $target" >&2
  exit 1
}
reload_marker="$target/runtime/.control-plane-reload-required"
pending_platform_upgrade=false
if [[ -e "$reload_marker" || -L "$reload_marker" ]]; then
  [[ -f "$reload_marker" && ! -L "$reload_marker" ]] || { echo '重载标记必须为普通文件' >&2; exit 1; }
  if [[ "${ASSETS_PLATFORM_UPGRADE_IMAGE:-}" =~ ^[^[:space:]]+@sha256:[a-f0-9]{64}$ ]]; then
    pending_platform_upgrade=true
    echo '受控平台镜像升级继续安装；保留门禁直到迁移和新控制面验证完成'
  else
    echo "检测到上一次部署资产更新尚未完成控制面成对重载：$reload_marker" >&2
    echo "请先执行：$target/bin/deploy.sh reload-control-plane，或执行受控平台镜像升级" >&2
    exit 1
  fi
fi

control_plane_running=false
if control_plane_is_running; then
  control_plane_running=true
else
  control_plane_status=$?
  ((control_plane_status == 1)) || exit "$control_plane_status"
fi

stage="$(mktemp -d "$target/.assets-install.XXXXXX")"
transaction_stage=''
compose_update=''
cleanup() {
  local status=$?
  trap - EXIT
  rm -rf -- "$stage"
  [[ -z "$transaction_stage" ]] || rm -rf -- "$transaction_stage"
  [[ -z "$compose_update" ]] || rm -f -- "$compose_update"
  if ((status != 0)) && [[ -d "$transaction" ]]; then
    echo "部署资产安装未完成；已阻止后续部署。请执行：$0 --recover $target" >&2
  fi
  exit "$status"
}
trap cleanup EXIT
tar -xzf "$archive" --no-same-owner --no-same-permissions -C "$stage"
[[ -f "$stage/docker-compose.yml" && -f "$stage/bin/deploy.sh" ]] || { echo '资产缺少统一编排或部署入口' >&2; exit 1; }
bash -n "$stage/bin/deploy.sh"

# Preserve operator-selected services. Only the known managed Docker Hub proxy
# reference changes registry; version, proxy ACLs and every other YAML line stay.
if [[ -f "$target/docker-compose.yml" ]]; then
  [[ ! -L "$target/docker-compose.yml" ]] || { echo '统一编排不能是符号链接' >&2; exit 1; }
  compose_update="$(mktemp "$target/.compose-managed-image.XXXXXX")"
  cp -p -- "$target/docker-compose.yml" "$compose_update"
  awk '
    /^  docker-socket-proxy:[[:space:]]*(#.*)?$/ {proxy=1; print; next}
    /^  [^[:space:]#]/ {proxy=0}
    proxy && /^    image:[[:space:]]/ {
      image=$2
      gsub(/["\047]/, "", image)
      if (image=="tecnativa/docker-socket-proxy:v0.5.0" || image=="docker.io/tecnativa/docker-socket-proxy:v0.5.0")
        sub(/(docker[.]io\/)?tecnativa\/docker-socket-proxy:v0[.]5[.]0/,
            "ghcr.io/tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459")
    }
    {print}
  ' "$target/docker-compose.yml" >"$compose_update"
  if cmp -s -- "$target/docker-compose.yml" "$compose_update"; then
    rm -f -- "$compose_update"
    compose_update=''
  fi
fi

backup="$(mktemp -d "$target/backups/install-assets.XXXXXX")"
transaction_stage="$(mktemp -d "$target/runtime/.assets-install-transaction.prepare.XXXXXX")"
: > "$transaction_stage/touched"
: > "$transaction_stage/existing"
printf '%s\n' "$backup" > "$transaction_stage/backup"
printf '%s\n' "$actual" > "$transaction_stage/archive-sha256"
if [[ "$pending_platform_upgrade" == true ]]; then
  cp -p -- "$reload_marker" "$transaction_stage/previous-reload-marker"
fi
printf 'prepared\n' > "$transaction_stage/state"
if [[ -n "$compose_update" ]]; then
  printf 'docker-compose.yml\n' >> "$transaction_stage/touched"
  printf 'docker-compose.yml\n' >> "$transaction_stage/existing"
  cp -a -- "$target/docker-compose.yml" "$backup/docker-compose.yml"
fi

shopt -s dotglob nullglob
for source in "$stage"/*; do
  name="$(basename "$source")"
  destination_name="$name"
  if [[ "$name" == docker-compose.yml && -f "$target/docker-compose.yml" ]]; then
    destination_name=docker-compose.yml.dist
  fi
  safe_top_name "$destination_name" || { echo "资产顶层路径未登记：$destination_name" >&2; exit 1; }
  printf '%s\n' "$destination_name" >> "$transaction_stage/touched"
  if [[ -e "$target/$destination_name" || -L "$target/$destination_name" ]]; then
    [[ ! -L "$target/$destination_name" ]] || { echo "目标资产不能为符号链接：$destination_name" >&2; exit 1; }
    if [[ -d "$target/$destination_name" ]] && [[ -n "$(find "$target/$destination_name" -type l -print -quit)" ]]; then
      echo "目标资产目录包含符号链接：$destination_name" >&2; exit 1
    fi
    cp -a -- "$target/$destination_name" "$backup/$destination_name"
    printf '%s\n' "$destination_name" >> "$transaction_stage/existing"
  fi
done
mv -- "$transaction_stage" "$transaction"
transaction_stage=''
printf 'applying\n' > "$transaction/state"

for source in "$stage"/*; do
  name="$(basename "$source")"
  destination_name="$name"
  if [[ "$name" == docker-compose.yml && -f "$target/docker-compose.yml" ]]; then
    destination_name=docker-compose.yml.dist
  fi
  if [[ -d "$source" ]]; then
    rm -rf -- "$target/$destination_name"
    mv -- "$source" "$target/$destination_name"
  else
    file_temporary="$(mktemp "$target/.${destination_name}.install.XXXXXX")"
    cp -p -- "$source" "$file_temporary"
    mv -f -- "$file_temporary" "$target/$destination_name"
  fi
done

if [[ -n "$compose_update" ]]; then
  mv -f -- "$compose_update" "$target/docker-compose.yml"
  compose_update=''
  echo '已定向更新受管 Docker Socket Proxy 为同版官方 GHCR 不可变镜像'
fi

# 非 root 容器需能遍历其只读 bind mount；只调整非敏感部署资产。
for name in subsystems.d subsystem-templates mysql-init nginx monitoring; do
  if [[ -d "$target/$name" ]]; then
    find "$target/$name" -type d -exec chmod 755 {} +
    find "$target/$name" -type f -exec chmod 644 {} +
  fi
done
chmod 750 "$target/bin/"*.sh
if [[ "$control_plane_running" == true || "$pending_platform_upgrade" == true ]]; then
  write_control_plane_reload_marker
fi
printf 'committed\n' > "$transaction/state"
rm -rf -- "$transaction"

echo "部署资产已安装到：$target"
echo "旧资产备份：$backup"
if [[ -f "$target/docker-compose.yml.dist" ]]; then
  echo '保留了当前 docker-compose.yml 的模块裁剪；新模板在 docker-compose.yml.dist，请合并需要的编排更新。'
fi
if [[ "$control_plane_running" == true || "$pending_platform_upgrade" == true ]]; then
  echo "检测到运行中的控制面；已写入强制重载标记：$reload_marker"
  if [[ -n "${ASSETS_PLATFORM_UPGRADE_IMAGE:-}" ]]; then
    echo '必须继续执行受控平台镜像发布：先完成迁移，再验证新 Agent/API 后清除门禁'
  else
    echo "必须先执行：$target/bin/deploy.sh reload-control-plane"
  fi
  echo '重载成功前，deploy.sh 会拒绝导入、准备、更新和继续部署。'
else
  echo '未启动容器。首次安装先 configure，再使用 deploy.sh start；旧环境先核对配置与镜像版本。'
fi
