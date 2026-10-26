#!/usr/bin/env bash
# =============================================================================
# 统一离线部署入口（部署目录内使用）
#
#   运行位置：部署目录，即本文件所在 bin/ 的上一级同时存在 docker-compose.yml、
#             .env、.release.env、subsystems.d/ 的目录（默认 /opt/unified-identity-platform）。
#   前置条件：已解压 deployment-assets-*.tar.gz，并至少执行过一次 configure。
#   完整流程：见同目录 OFFLINE_DEPLOYMENT.md；子命令总览执行 ./bin/deploy.sh help。
#   注意：    交付目录顶层的“参考副本”不能替代本文件运行；本文件被移出完整
#             部署目录时会直接给出缺少哪些文件的明确错误。
# =============================================================================
set -Eeuo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd -- "$script_dir/.." && pwd)"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
manifest_dir="$deploy_dir/manifests"
package_dir="$deploy_dir/packages"
registry_address="${UIP_OFFLINE_REGISTRY:-127.0.0.1:5000}"
compose_file="$deploy_dir/docker-compose.yml"
profiles_dir="$deploy_dir/subsystems.d"
asset_install_transaction="$deploy_dir/runtime/.assets-install-transaction"
control_plane_reload_marker="$deploy_dir/runtime/.control-plane-reload-required"
if [[ -e "$asset_install_transaction" || -L "$asset_install_transaction" ]]; then
  if [[ "${1:-}" == "plan" ]]; then
    shift
    exec python3 "$script_dir/deployment-plan.py" --target "$deploy_dir" "$@"
  fi
  printf '错误：检测到未完成的部署资产安装事务：%s\n' "$asset_install_transaction" >&2
  printf '拒绝在脚本/清单可能混合的状态下继续部署。请使用同一交付包中的 install-assets.sh --recover %s 恢复后重试。\n' "$deploy_dir" >&2
  exit 1
fi
source "$script_dir/deploy-common.sh"
source "$script_dir/deploy-import.sh"
source "$script_dir/deploy-subsystem.sh"
source "$script_dir/deploy-license.sh"
source "$script_dir/deploy-doctor.sh"
source "$script_dir/deploy-package.sh"
source "$script_dir/deploy-status.sh"
source "$script_dir/deploy-verify.sh"
source "$script_dir/deploy-menu.sh"
source "$script_dir/deploy-registry.sh"
source "$script_dir/compose-scope.sh"
source "$script_dir/start-enabled.sh"
source "$script_dir/offline-package-metadata.sh"
if [[ -f "$script_dir/deployment-workflow.sh" && ! -L "$script_dir/deployment-workflow.sh" ]]; then
  source "$script_dir/deployment-workflow.sh"
else
  [[ -z "${UIP_INSTALL_SESSION:-}" ]] || { printf '缺少匹配的安装会话工具，请使用完整新版资产。\n' >&2; exit 1; }
  workflow_enabled() { return 1; }
fi
if [[ -f "$script_dir/delivery-runtime.sh" && ! -L "$script_dir/delivery-runtime.sh" ]]; then
  source "$script_dir/delivery-runtime.sh"
fi

usage() {
  cat <<'EOF'
用法：./bin/deploy.sh [子命令] [参数]

不带子命令时进入中文交互菜单。

部署主流程（按此顺序）：
  configure                     一次性运行配置：IP、端口、管理员、时区、防火墙
  plan --action <install|upgrade|add|repair|resume> [--component <组件>] [--format human|json]
                                [--compatibility <声明>] [--baseline-report <文件>] [--forward-recovery --recovery-evidence <文件>]
                                只读生成统一预检、执行阶段和恢复边界；不改运行状态
  install                       扫描 packages/ 并自动部署基础平台/前端、准备已提供的子系统包
                                带客户许可证及批准清单时继续受控接入并等待授权确认
  delivery-proof <组件> <包> [公共包]
                                核验本次交付对应的实际发行与运行状态（恢复流程使用）
  delivery-prepare <组件> <包>  仅恢复同批次候选；待平台操作时保留候选并给出下一步
  license-renew <许可证路径>   导入同实例安全续期并等待组件确认，不重打业务镜像
  timezone-apply                离线应用已保存的显示时区配置；不改数据库时间或系统时钟
  import <镜像包路径>           导入镜像包，校验摘要并登记不可变 digest
  deploy platform               发布基础平台（含数据库迁移与首个管理员初始化）
  deploy frontend               发布统一前端
  start                         分阶段启动公共平台及 YAML 内已接入子系统
  resume <子系统>               恢复单个已接入子系统：依赖健康等待、备份、迁移与启动
  disable <子系统>              显式停止单系统，保留数据卷与密钥
  status [子系统]               查看容器状态；带子系统时显示阶段与唯一下一步
  verify [子系统]               健康检查；带子系统时对该子系统做部署验收
  logs <应用编码>               查看该应用的全部容器日志

业务子系统接入（每个子系统严格按此顺序，不可跳步）：
  import <包> -> prepare <子系统> -> 在平台页面采用 <应用编码>/prod
              -> status <子系统> -> continue <子系统> -> verify <子系统>

日常运维：
  upgrade <组件>                暂存新版本镜像 digest，再由平台页面受控更新
  license-prepare              按已安装授权计划冻结真实旧范围；失败禁止更新业务
  license-migrate              按整个数据库冻结旧集合受控迁移并等待当前确认
  license-activate             激活整个随包签署购买集合并等待全部必需组件当前 ACK
                                无 JWS 的存量迁移不会自动获得新系统授权
  doctor [子系统]               只读环境自检：配置权限、镜像包摘要、导入记录与
                                .release.env 一致性、Agent 绑定、runtime 占位凭据；
                                带子系统时显示当前阶段与下一步
  reload-control-plane          部署资产变更后成对重建 Agent 与 platform-api，并校验
  recover-platform <包>         从标准 platform 包协调恢复控制面，保留迁移和健康门禁
                                subsystems.d 集合摘要、单文件挂载、Socket 与健康状态
  backup                        对所有运行中的 MySQL 做逻辑备份并校验摘要
  restore --service <服务> --backup <文件> --verify-only
  restore --service <服务> --backup <文件> --confirm RESTORE_MYSQL_SERVICE

支持的子系统参数：
  customer-opportunity  客户与商机管理系统
  customer-portal       客户自助门户
  contract              合同管理系统
  project               项目服务内容管理系统
  settlement            结算与开票管理系统
  data-analysis         数据看板与统计分析系统

help                          显示本帮助
EOF
}

# Loading the facade for its deployment functions must not enter its CLI.
[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0

command="${1:-menu}"
if [[ -e "$control_plane_reload_marker" || -L "$control_plane_reload_marker" ]]; then
  case "$command" in
    reload-control-plane|doctor|status|plan|logs|help|--help|-h) ;;
    import|recover-platform|delivery-import-proof) ;; # Pure import proofs do not publish or reload.
    deploy)
      [[ "${2:-}" == platform && "${DEPLOY_PLATFORM_CONTROL_PLANE_UPGRADE:-false}" == true ]] ||
        die '待完成控制面升级仅允许显式协调的 platform 发布'
      ;;
    *)
      die "检测到部署资产已更新但子系统控制面尚未成对重载：$control_plane_reload_marker
为防止 platform-api 与 subsystem-provisioner 使用不同清单摘要，当前仅允许只读诊断。
请先执行：$0 reload-control-plane"
      ;;
  esac
fi
case "$command" in
  menu) menu ;;
  configure) shift; configure "$@" ;;
  plan)
    shift
    exec python3 "$script_dir/deployment-plan.py" --target "$deploy_dir" "$@"
    ;;
  install) auto_install_packages ;;
  delivery-import-proof)
    [[ $# == 3 ]] || die 'delivery-import-proof 需要组件和本次镜像包'
    delivery_import_matches "$2" "$3"
    ;;
  delivery-proof)
    [[ $# == 3 || $# == 4 ]] || die 'delivery-proof 需要组件、本次镜像包及平台所需公共包'
    require_initialized
    delivery_release_matches "$2" "$3" "${4:-}"
    ;;
  delivery-final-proof)
    [[ $# == 3 ]] || die 'delivery-final-proof 需要业务组件和本次镜像包'
    delivery_release_matches "$2" "$3" || exit 1
    verify_subsystem_current_health "$2"
    ;;
  delivery-prepare)
    [[ $# == 3 ]] || die 'delivery-prepare 需要组件和本次镜像包'
    delivery_prepare_resume "$2" "$3"
    ;;
  delivery-enabled)
    [[ $# == 2 ]] || die 'delivery-enabled 需要组件'
    scope_services >/dev/null || die '无法读取当前组件启用范围'
    if scope_enabled "$2"; then printf 'ENABLED\n'; else printf 'DISABLED\n'; fi
    ;;
  delivery-license-proof)
    [[ $# == 1 ]] || die 'delivery-license-proof 不接受授权覆盖参数'
    require_initialized
    compose run -T --rm --no-deps platform-api ./license-install verify-enforcement
    ;;
  license-renew) renew_delivery_license "${2:-}" ;;
  license-prepare)
    [[ $# == 1 ]] || die 'license-prepare 只读取已安装计划，不接受客户或场景覆盖参数'
    require_initialized
    prepare_license_installation
    ;;
  license-migrate)
    [[ $# == 1 ]] || die 'license-migrate 只处理已冻结的完整旧集合，不接受选择或替换基线'
    require_initialized
    [[ -f "$deploy_dir/license-installation.json" && ! -L "$deploy_dir/license-installation.json" ]] || die '缺少安全授权安装计划，请先 license-prepare'
    compose run -T --rm --no-deps platform-api ./license-install migrate
    printf '存量迁移确认完成；过渡资格不等于正式授权。\n'
    ;;
  license-activate)
    [[ $# == 1 ]] || die 'license-activate 只使用已安装许可证，不接受隐式更换签署购买集合'
    require_initialized
    install_delivery_license activate
    printf '授权激活完成；整个签署购买集合的必需组件已确认当前快照。\n'
    ;;
  timezone-apply) UIP_TIMEZONE_CONTROLLED_APPLY=1 bash "$script_dir/set-container-timezone.sh" internal-apply --dir "$deploy_dir" ;;
  start) start_enabled ;;
  resume) require_initialized; start_registered "${2:?请指定子系统}" ;;
  disable) disable_component "${2:?请指定子系统}" ;;
  import) import_package "${2:-}" ;;
  deploy) deploy_component "${2:-}" ;;
  recover-platform)
    [[ -f "${2:-}.sha256" && ! -L "${2:-}.sha256" ]] || die '平台恢复必须显式提供安全 SHA256 伴随文件'
    import_package "${2:?请指定经过校验的 platform 离线镜像包}" platform
    DEPLOY_PLATFORM_CONTROL_PLANE_UPGRADE=true deploy_component platform
    verify
    ;;
  prepare) prepare_subsystem "${2:-}" ;;
  continue) continue_subsystem "${2:-}" ;;
  upgrade) stage_upgrade "${2:-}" ;;
  reload-control-plane) reload_control_plane ;;
  doctor) doctor "${2:-}" ;;
  status) status "${2:-}" ;;
  logs) logs "${2:-}" ;;
  verify) verify "${2:-}" ;;
  backup) backup_all ;;
  restore) shift; "$script_dir/restore-mysql.sh" "$@" ;;
  help|--help|-h) usage ;;
  *) die "未知命令：${command}（执行 $0 help 查看可用子命令）" ;;
esac
