#!/usr/bin/env bash
# 成套升级平台能力；业务子系统仍须走平台受控更新。
set -Eeuo pipefail
umask 077
die() { printf '[错误] %s\n' "$*" >&2; exit 1; }
usage() { printf '%s\n' '用法：bash license-upgrade.sh --image-tar platform-backend-V-linux-amd64.tar.gz [--deploy-dir DIR]' '接受标准离线包及同名 .sha256，不接受裸 docker-save。先安装同版资产及公共基础设施包。' '平台 API/Worker/Agent/迁移成套升级；不代表业务组件全部升级或授权已激活。'; }
deploy_dir=/opt/unified-identity-platform
image_tar=''
while (($#)); do
  case "$1" in
    --image-tar|--deploy-dir)
      [[ $# -ge 2 && -n "$2" ]] || die "$1 缺少参数"
      case "$1" in --image-tar) image_tar=$2;; --deploy-dir) deploy_dir=$2;; esac
      shift 2;;
    -h|--help) usage; exit 0;;
    *) die "未知参数：$1";;
  esac
done
[[ $(id -u) == 0 ]] || die '请由部署管理员以 root 运行'
[[ -f "$image_tar" && ! -L "$image_tar" ]] || die '平台包必须是非符号链接常规文件'
case "$(basename -- "$image_tar")" in platform-backend-*-linux-amd64.tar.gz) ;; *) die '不是标准平台离线包';; esac
[[ -f "$image_tar.sha256" && ! -L "$image_tar.sha256" ]] || die '缺少同名 .sha256 校验文件'
image_tar="$(cd -- "$(dirname -- "$image_tar")" && pwd)/$(basename -- "$image_tar")"
[[ -d "$deploy_dir" && ! -L "$deploy_dir" ]] || die '部署目录无效'
cd -- "$deploy_dir"
[[ -f bin/deploy.sh && ! -L bin/deploy.sh ]] || die '缺少受控部署入口'
[[ ! -e runtime/.control-plane-reload-required && ! -L runtime/.control-plane-reload-required ]] || die '控制面尚未完成重载；使用 forward-recover.sh，不删除门禁标记'
bash bin/deploy.sh import "$image_tar"
bash bin/deploy.sh upgrade platform
bash bin/deploy.sh verify
printf '[信息] 平台成套能力升级及健康检查完成；尚不代表六系统商业授权激活或业务验收通过。\n'
