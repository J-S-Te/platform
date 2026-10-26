#!/usr/bin/env bash
# Deploy menu/log/backup helpers. Sourced by deploy.sh.

logs() {
  local component="${1:?component required}"
  docker ps -a --filter "label=com.basic-platform.application_code=$component" --format '{{.Names}}' | while read -r name; do
    [[ -n "$name" ]] || continue
    printf '\n===== %s =====\n' "$name"
    docker logs --tail 200 "$name"
  done
}

backup_all() {
  require_initialized
  "$script_dir/backup-all.sh"
}

disable_component() (
  require_initialized
  need flock
  [[ ! -L "$deploy_dir/runtime/.deploy.lock" ]] || die '部署锁不能是符号链接'
  exec 7>"$deploy_dir/runtime/.deploy.lock"
  flock -w 30 7 || die '平台接入或其他发布正在进行，请稍后停用'
  [[ "${1:-}" != project ]] || project_background_teardown || return
  scope_disable "${1:?请指定子系统}" || return
  [[ "${1:-}" != project ]] || project_background_stopped
)

restore_menu() {
  local service backup answer
  read -r -p 'MySQL 服务名称：' service
  read -r -p '备份 .sql.gz 文件路径：' backup
  "$script_dir/restore-mysql.sh" --service "$service" --backup "$backup" --verify-only
  read -r -p '恢复会覆盖数据库。确认继续请输入 RESTORE_MYSQL_SERVICE：' answer
  [[ "$answer" == RESTORE_MYSQL_SERVICE ]] || { echo '已取消恢复'; return 0; }
  "$script_dir/restore-mysql.sh" --service "$service" --backup "$backup" --confirm RESTORE_MYSQL_SERVICE
}

menu() {
  while true; do
    printf '\n统一身份认证平台离线部署\n1. 初始化部署配置\n2. 导入镜像包\n3. 部署基础平台\n4. 部署统一前端\n5. 准备子系统供平台探测\n6. 完成已接入子系统部署\n7. 升级已部署模块\n8. 查看容器状态\n9. 查看模块日志\n10. 执行健康检查\n11. 备份\n12. 恢复\n13. 环境自检（doctor，只读）\n14. 自动安装 packages/ 中的镜像包\n15. 成对重载子系统控制面\n16. 生成只读部署计划（plan）\n0. 退出\n'
    read -r -p '请选择操作：' choice
    case "$choice" in
      1) configure ;;
      2) read -r -p '镜像包路径：' path; import_package "$path" ;;
      3) deploy_component platform ;;
      4) deploy_component frontend ;;
      5) read -r -p '子系统参数：' name; prepare_subsystem "$name" ;;
      6) read -r -p '子系统参数：' name; continue_subsystem "$name" ;;
      7) read -r -p '组件参数：' name; stage_upgrade "$name" ;;
      8) read -r -p '子系统参数（直接回车查看全部容器）：' name; status "$name" ;;
      9) read -r -p '应用编码：' name; logs "$name" ;;
      10) verify ;;
      11) backup_all ;;
      12) restore_menu ;;
      13) read -r -p '子系统参数（直接回车检查全部）：' name; doctor "$name" || true ;;
      14) auto_install_packages ;;
      15) reload_control_plane ;;
      16) read -r -p '操作（install/upgrade/add/repair/resume）：' action; read -r -p '组件（直接回车 all）：' component; if [[ -n "$component" ]]; then bash "$script_dir/deploy.sh" plan --action "$action" --component "$component" --format human; else bash "$script_dir/deploy.sh" plan --action "$action" --format human; fi ;;
      0) return ;;
      *) echo '选择无效，请重新输入' >&2 ;;
    esac
  done
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0
