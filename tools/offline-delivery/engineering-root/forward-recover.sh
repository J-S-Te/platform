#!/usr/bin/env bash
# 保留门禁和安装身份，恢复由生产发布器完整执行。
set -Eeuo pipefail
umask 077
die() { printf '[错误] %s\n' "$*" >&2; exit 1; }
usage() { printf '%s\n' '用法：bash forward-recover.sh MEDIA_DIR [--deploy-dir DIR] [--platform-package PATH]' '多版本介质必须明确指定平台包；不会手改版本引用或删除门禁。'; }
deploy_dir=/opt/unified-identity-platform
media_dir=''
platform_pkg=''
while (($#)); do
  case "$1" in
    --deploy-dir|--platform-package)
      [[ $# -ge 2 && -n "$2" ]] || die "$1 缺少参数"
      case "$1" in --deploy-dir) deploy_dir=$2;; --platform-package) platform_pkg=$2;; esac
      shift 2;;
    -h|--help) usage; exit 0;;
    -*) die "未知参数：$1";;
    *) [[ -z "$media_dir" ]] || die '只能指定一个介质目录'; media_dir=$1; shift;;
  esac
done
[[ $(id -u) == 0 ]] || die '请由部署管理员以 root 运行'
[[ -d "$media_dir" && ! -L "$media_dir" ]] || die '介质目录无效'
if [[ -z "$platform_pkg" ]]; then
  shopt -s nullglob
  candidates=("$media_dir"/platform-backend-*-linux-amd64.tar.gz)
  shopt -u nullglob
  ((${#candidates[@]} == 1)) || die '必须恰好有一个平台包；多个版本请使用 --platform-package 指定'
  platform_pkg=${candidates[0]}
fi
[[ -f "$platform_pkg" && ! -L "$platform_pkg" ]] || die '平台包无效'
case "$(basename -- "$platform_pkg")" in platform-backend-*-linux-amd64.tar.gz) ;; *) die '不是标准平台离线包';; esac
[[ -f "$platform_pkg.sha256" && ! -L "$platform_pkg.sha256" ]] || die '缺少平台包校验文件'
platform_pkg="$(cd -- "$(dirname -- "$platform_pkg")" && pwd)/$(basename -- "$platform_pkg")"
[[ -d "$deploy_dir" && ! -L "$deploy_dir" ]] || die '部署目录无效'
cd -- "$deploy_dir"
[[ -f bin/deploy.sh && ! -L bin/deploy.sh ]] || die '缺少受控生产部署入口'
bash bin/deploy.sh help | grep -q 'recover-platform' || die '当前资产不支持 recover-platform；先安装同版资产，不得手改版本或删除门禁'
bash bin/deploy.sh recover-platform "$platform_pkg"
printf '[信息] 平台前向恢复完成；前端、业务子系统与授权仍需各自受控升级和执行确认。\n'
