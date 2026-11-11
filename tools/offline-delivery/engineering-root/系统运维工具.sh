#!/usr/bin/env bash
# 统一身份认证平台安全运维入口；复用既有受控部署、备份和验收能力。
set -Eeuo pipefail
umask 077
target_dir="/opt/unified-identity-platform"

# 用途：显示运维命令、影响范围及确认要求。
# 参数：无。
# 返回值：无。
usage() {
  cat <<'EOF'
用法：./系统运维工具.sh [--target <部署目录>] [命令] [参数]
不带命令进入中文菜单；Linux服务器上请显式使用sudo，不自动提权。

只读元数据与诊断：
  overview                         宿主资源和本安装容器概览，不调用部署验收
  resources                        宿主资源及本安装容器瞬时统计
  logs <platform|frontend|子系统>   最近200行日志，可能含业务数据，请勿直接外发
  collect [输出绝对目录]            采集元数据，不调用doctor/status/verify或收集日志
  plan [install|upgrade|add|repair|resume] [组件]
  verify-backup <备份批次目录>      校验现有灾备批次

部署检查（底层可能准备网络、创建一次性验收容器，须明确确认）：
  doctor [子系统] [RUN_DEPLOY_CHECKS]
  status [子系统] [RUN_DEPLOY_CHECKS]
  verify [子系统] [RUN_DEPLOY_CHECKS]

会改变运行状态（无交互必须提供精确确认词）：
  backup [CREATE_BACKUP]
  repair <platform|frontend|all|子系统> [REPAIR_CURRENT_VERSION]
  disable <子系统> [DISABLE_KEEP_DATA]
  resume <子系统> [RESUME_SUBSYSTEM]
  continue <子系统> [CONTINUE_CONTROLLED_DEPLOYMENT]
  upgrade <业务子系统> <新镜像包> [STAGE_UPGRADE]
      导入并暂存，之后仍需平台受控更新
  upgrade <platform|frontend> <新镜像包> [PUBLISH_CORE_UPGRADE]
      实际发布核心组件，可能备份、迁移和重建服务；不是仅暂存

子系统：customer-opportunity, customer-portal, contract, project, settlement, data-analysis
仅接受本机Unix socket Docker；核对项目和安装目录归属，不按容器名前缀管理。
不提供恢复数据库、清理、卸载、删除数据卷或Docker prune。
已开始的变更失败会返回非零；不会自动回滚数据库或已经完成的步骤。
EOF
}

# 用途：打印错误并结束当前操作。
# 参数：所有参数为可公开的错误说明。
# 返回值：退出码1。
die() { printf '[错误] %s\n' "$*" >&2; exit 1; }

# 用途：打印执行结果或操作提示。
# 参数：所有参数为可公开的信息。
# 返回值：printf的退出码。
info() { printf '[信息] %s\n' "$*"; }

# 用途：打印需要操作员注意的限制。
# 参数：所有参数为可公开的提醒。
# 返回值：printf的退出码。
warn() { printf '[警告] %s\n' "$*" >&2; }

# 用途：检查所需命令是否已安装。
# 参数：第一个参数为命令名。
# 返回值：命令存在返回0，否则退出1。
need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

# 用途：执行有限路径、权限、归属和包校验，不求值.env。
# 参数：操作名及对应文件路径、元数据或文件描述符。
# 返回值：仅输出项目、摘要或已校验容器ID；失败返回1。
guard_python() {
  python3 - "$@" <<'PY'
import hashlib, json, os, re, stat, sys, tarfile
from pathlib import Path

# 用途：验证绝对规范路径及父目录，拒绝符号链接。
# 参数：value为路径字符串。
# 返回值：Path对象；不安全时抛出异常。
def canonical(value):
    path = Path(value)
    if (not path.is_absolute() or str(path) != value or '..' in path.parts or
            any(ord(c) < 32 or ord(c) == 127 for c in value)):
        raise ValueError('路径必须是规范绝对路径')
    for item in (path, *path.parents):
        if item.is_symlink():
            raise ValueError('路径包含符号链接：'+str(item))
        if item.exists() and item.is_dir():
            meta = item.stat()
            if meta.st_uid not in (0, os.geteuid()):
                raise ValueError('父目录不属于root或当前用户：'+str(item))
            if meta.st_mode & 0o022 and not meta.st_mode & stat.S_ISVTX:
                raise ValueError('父目录允许其他用户写入：'+str(item))
    return path

# 用途：验证有限大小普通文件及执行者所有权。
# 参数：path为路径，secret表示要求0600，limit为字节上限。
# 返回值：文件字节；类型或权限异常时抛错。
def regular(path, secret=False, limit=16*1024*1024):
    canonical(str(path))
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, 'rb') as stream:
        meta = os.fstat(stream.fileno())
        if (not stat.S_ISREG(meta.st_mode) or meta.st_nlink != 1 or
                meta.st_uid != os.geteuid() or meta.st_size > limit or meta.st_mode & 0o022):
            raise ValueError('文件类型、大小、所有权或权限不安全：'+str(path))
        if secret and stat.S_IMODE(meta.st_mode) != 0o600:
            raise ValueError('配置权限必须为0600：'+str(path))
        return stream.read(limit+1)

# 用途：只读取项目名，不解释或输出其他配置值。
# 参数：raw为配置文件字节。
# 返回值：项目名或空字符串；重复、动态或非法项目名拒绝。
def project_value(raw):
    values = []
    for line in raw.decode('utf-8').split('\n'):
        match = re.match(r'^\s*(?:export\s+)?COMPOSE_PROJECT_NAME\s*=(.*)$', line)
        if match:
            if not re.fullmatch(r'COMPOSE_PROJECT_NAME=[a-z0-9][a-z0-9_-]{0,62}', line):
                raise ValueError('项目名须为规范原始赋值，禁止export、空白、引号或CRLF')
            values.append(match[1])
    if len(values) > 1 or values and not re.fullmatch(r'[a-z0-9][a-z0-9_-]{0,62}', values[0]):
        raise ValueError('项目名重复、为空、含动态表达式或格式无效')
    return values[0] if values else ''

# 用途：校验安装入口并绑定当前配置摘要。
# 参数：value为安装目录。
# 返回值：项目名及完整入口摘要的两行输出。
def installation(value):
    root = canonical(value)
    if (not root.is_dir() or len(root.parts) < 3 or
            any(root == p or p in root.parents for p in
                map(Path, ('/etc','/usr','/dev','/proc','/sys','/bin','/sbin','/lib'))) or
            root in (Path.home(), Path('/var/lib'), Path('/var/tmp'))):
        raise ValueError('不是安全的专用部署目录')
    if any((root/n).exists() for n in ('.git','.ai','.agents','AGENTS.md')):
        raise ValueError('目标疑似源码工作区，拒绝运行运维')
    if root.stat().st_uid != os.geteuid():
        raise ValueError('安装目录不属于当前执行用户')
    proof = hashlib.sha256()
    identity = root.stat()
    proof.update(str((identity.st_dev, identity.st_ino)).encode()+b'\0')
    projects = []
    for name in ('bin/deploy.sh','bin/lifecycle.sh','bin/backup-all.sh',
                 'docker-compose.yml','.env','.release.env'):
        raw = regular(root/name, secret=name in ('.env','.release.env'))
        proof.update(name.encode()+b'\0'+raw)
        if name in ('.env','.release.env'):
            projects.append(project_value(raw))
    for path in sorted((root/'bin').rglob('*')):
        if path.suffix in ('.sh', '.py'):
            proof.update(str(path.relative_to(root)).encode()+b'\0'+regular(path))
    if projects[1] and projects[1] != projects[0]:
        raise ValueError('.release.env覆盖项目名，须先核对归属')
    print(projects[0] or 'basic-platform-production')
    print(proof.hexdigest())

# 用途：校验当前项目容器绑定本安装目录。
# 参数：target为部署路径，rows为Docker返回的脱敏JSON行。
# 返回值：已校验的容器ID；归属不明时拒绝。
def containers(target, rows):
    for line in rows.splitlines():
        ident, working, configs = json.loads(line)
        if not re.fullmatch(r'[a-f0-9]{64}', ident) or working != target or not configs:
            raise ValueError('同项目容器的ID、工作目录或编排归属不匹配')
        for name in configs.split(','):
            path = Path(name)
            if not path.is_absolute() or '..' in path.parts or Path(target) not in path.parents:
                raise ValueError('同项目容器引用外部编排，拒绝运维')
        print(ident)

# 用途：校验锁文件和打开描述符没有被替换。
# 参数：value为锁路径，descriptor为可选的已打开描述符。
# 返回值：无；类型、权限、链接或身份异常时拒绝。
def lock_file(value, descriptor=None):
    path = canonical(value)
    if not path.exists():
        if descriptor is not None:
            raise ValueError('打开后锁路径消失')
        return
    meta = path.stat()
    if (not stat.S_ISREG(meta.st_mode) or meta.st_nlink != 1 or meta.st_size or
            meta.st_uid != os.geteuid() or stat.S_IMODE(meta.st_mode) != 0o600):
        raise ValueError('锁必须为当前用户的0600空单链接普通文件')
    if descriptor is not None:
        opened = os.fstat(int(descriptor))
        if (opened.st_dev, opened.st_ino) != (meta.st_dev, meta.st_ino):
            raise ValueError('锁路径与描述符身份不一致')

# 用途：先核对归档摘要和组件，防止导入其他系统的包。
# 参数：value为镜像包路径，component为请求组件。
# 返回值：无；摘要或组件不一致时拒绝，不解压文件。
def package(value, component):
    path = canonical(value)
    meta = path.stat()
    if (not stat.S_ISREG(meta.st_mode) or meta.st_nlink != 1 or
            meta.st_uid != os.geteuid() or meta.st_mode & 0o022):
        raise ValueError('镜像包必须为当前用户拥有且不可被他人写入的普通单链接文件')
    sidecar = regular(Path(value+'.sha256'), limit=65536).decode('utf-8')
    match = re.fullmatch(r'([a-fA-F0-9]{64})[ \t]+([^\r\n]+)\n?', sidecar)
    if not match or match[2] != path.name:
        raise ValueError('镜像包摘要文件必须只绑定本包')
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024*1024), b''):
            digest.update(chunk)
    if digest.hexdigest() != match[1].lower():
        raise ValueError('镜像包摘要不匹配')
    with tarfile.open(path, mode='r|gz') as archive:
        for entry in archive:
            if entry.name == 'package.env':
                if not entry.isfile() or entry.size > 65536:
                    raise ValueError('包元数据不是有限普通文件')
                raw = archive.extractfile(entry).read(65537).decode('utf-8')
                values = re.findall(r'^COMPONENT=([a-z0-9-]+)$', raw, re.M)
                if values != [component]:
                    raise ValueError('包内组件与请求组件不一致')
                return
    raise ValueError('镜像包缺少package.env')

try:
    mode, args = sys.argv[1], sys.argv[2:]
    if mode == 'installation':
        installation(args[0])
    elif mode == 'containers':
        containers(*args)
    elif mode == 'lock':
        lock_file(*args)
    elif mode == 'package':
        package(*args)
    elif mode == 'directory':
        path = canonical(args[0])
        if not path.is_dir():
            raise ValueError('目录不存在')
    elif mode == 'output':
        path = canonical(args[0])
        target = Path(args[1])
        if path == target or target in path.parents or path in target.parents:
            raise ValueError('诊断目录不得与安装目录重叠')
        if not path.parent.is_dir() or path.exists():
            raise ValueError('输出父目录须存在，输出目录不能已存在')
    else:
        raise ValueError('不支持的校验操作')
except (OSError, ValueError, KeyError, TypeError, tarfile.TarError) as error:
    print('校验失败：'+str(error), file=sys.stderr)
    sys.exit(1)
PY
}

# 用途：要求显式选择服务器权限，避免自动sudo执行。
# 参数：无。
# 返回值：Linux root返回0，否则退出1。
require_server() {
  [[ "$(uname -s)" == Linux ]] || die '运维工具只能在Linux部署服务器上运行'
  ((EUID == 0)) || die '请核对目标后显式使用sudo执行；本工具不自动提权'
}

# 用途：恢复安装入口及项目名，校验配置权限。
# 参数：无，使用target_dir。
# 返回值：成功更新入口变量，否则返回非零。
load_installation() {
  local metadata
  metadata="$(guard_python installation "$target_dir")" || return
  compose_project="${metadata%%$'\n'*}"
  installation_fingerprint="${metadata#*$'\n'}"
  deploy="$target_dir/bin/deploy.sh"
  lifecycle="$target_dir/bin/lifecycle.sh"
  backup_tool="$target_dir/bin/backup-all.sh"
  compose_file="$target_dir/docker-compose.yml"
  runtime_file="$target_dir/.env"
  release_file="$target_dir/.release.env"
}

# 用途：只接受本机Docker，防止本地目录配合远端daemon。
# 参数：无。
# 返回值：本机端点返回0，否则退出1。
require_local_docker() {
  local endpoint
  endpoint="$(docker context inspect --format '{{json .Endpoints.docker.Host}}')" || return
  endpoint="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1]))' "$endpoint")" || return
  if [[ -z "${DOCKER_CONTEXT:-}" ]]; then endpoint="${DOCKER_HOST:-$endpoint}"; fi
  [[ "$endpoint" == unix:///* ]] || die '拒绝远程Docker context/DOCKER_HOST；仅允许本机Unix socket'
}

# 用途：获取本安装容器ID，拒绝同项目名的其他部署。
# 参数：无。
# 返回值：已校验ID逐行输出；Docker或归属检查失败返回非零。
owned_container_ids() {
  local values rows ident
  local -a ids=()
  values="$(docker ps -aq --no-trunc --filter "label=com.docker.compose.project=$compose_project")" || return
  while IFS= read -r ident; do
    [[ -n "$ident" ]] || continue
    [[ "$ident" =~ ^[a-f0-9]{64}$ ]] || die 'Docker返回非法容器ID'
    ids+=("$ident")
  done <<<"$values"
  (("${#ids[@]}" > 0)) || return 0
  rows="$(docker container inspect --format \
    '[{{json .Id}},{{json (index .Config.Labels "com.docker.compose.project.working_dir")}},{{json (index .Config.Labels "com.docker.compose.project.config_files")}}]' \
    "${ids[@]}")" || return
  guard_python containers "$target_dir" "$rows"
}

# 用途：执行底层部署命令，固定项目及配置来源。
# 参数：底层部署子命令和参数。
# 返回值：底层真实退出码。
deploy_run() {
  owned_container_ids >/dev/null || return
  (cd "$target_dir" && COMPOSE_PROJECT_NAME="$compose_project" \
    COMPOSE_PROFILES= COMPOSE_FILE= bash "$deploy" "$@")
}

# 用途：执行受控生命周期操作，不提供删除入口。
# 参数：已校验的repair命令及目标。
# 返回值：底层真实退出码。
lifecycle_run() {
  owned_container_ids >/dev/null || return
  (cd "$target_dir" && COMPOSE_PROJECT_NAME="$compose_project" \
    COMPOSE_PROFILES= COMPOSE_FILE= bash "$lifecycle" "$@")
}

# 用途：协调安装长锁和运维锁，保留底层发布/备份锁。
# 参数：要执行的函数及参数。
# 返回值：真实退出码；占用、清理中或身份变化时拒绝。
run_exclusive() (
  local baseline="$installation_fingerprint" lock
  need flock || return
  [[ ! -e "$target_dir/.first-install-reset.json" && ! -L "$target_dir/.first-install-reset.json" ]] ||
    die '存在未完成首装重置；请先按清理记录恢复，不运行其他变更'
  guard_python directory "$target_dir/runtime" || return
  lock="${target_dir}.installation-run.lock"
  guard_python lock "$lock" || return
  exec 6<>"$lock" || return
  guard_python lock "$lock" 6 || return
  flock -n 6 || die '安装或失败首装清理正在运行'
  lock="$target_dir/runtime/.operations.lock"
  guard_python lock "$lock" || return
  exec 5<>"$lock" || return
  guard_python lock "$lock" 5 || return
  flock -n 5 || die '另一个运维操作正在运行'
  load_installation || return
  [[ "$baseline" == "$installation_fingerprint" ]] || die '等待锁期间配置或入口已变化，请重新核对后执行'
  [[ ! -e "$target_dir/.first-install-reset.json" && ! -L "$target_dir/.first-install-reset.json" ]] ||
    die '取得锁后发现未完成首装重置，拒绝执行变更'
  owned_container_ids >/dev/null || return
  "$@"
)

# 用途：校验业务子系统白名单。
# 参数：第一个参数为子系统名称。
# 返回值：合法返回0，否则退出1。
validate_subsystem() {
  case "${1:-}" in
    customer-opportunity|customer-portal|contract|project|settlement|data-analysis) ;;
    *) die "不支持的子系统：${1:-<空>}" ;;
  esac
}

# 用途：校验可修复目标。
# 参数：第一个参数为组件或all。
# 返回值：合法返回0，否则退出1。
validate_repair_target() {
  case "${1:-}" in platform|frontend|all) ;; *) validate_subsystem "${1:-}" ;; esac
}

# 用途：校验可升级组件。
# 参数：第一个参数为核心组件或子系统。
# 返回值：合法返回0，否则退出1。
validate_upgrade_component() {
  case "${1:-}" in platform|frontend) ;; *) validate_subsystem "${1:-}" ;; esac
}

# 用途：转换底层日志接口所需应用编码。
# 参数：第一个参数为业务子系统。
# 返回值：应用编码；不支持时返回1。
application_code() {
  case "$1" in
    customer-opportunity) printf 'customer_and_opportunity' ;;
    customer-portal) printf 'customer_portal' ;;
    contract) printf 'contract_management' ;;
    project) printf 'project_management' ;;
    settlement) printf 'settlement' ;;
    data-analysis) printf 'data_analysis' ;;
    *) return 1 ;;
  esac
}

# 用途：核对精确确认词，非交互时不默认批准。
# 参数：expected为确认词，supplied为输入，message为操作影响。
# 返回值：匹配返回0，否则退出1。
confirm_exact() {
  local expected="$1" supplied="${2:-}" message="${3:-继续执行}"
  if [[ -z "$supplied" && -t 0 ]]; then
    printf '%s\n请输入确认词 %s：' "$message" "$expected" >&2
    IFS= read -r supplied || return 1
  fi
  [[ "$supplied" == "$expected" ]] || die "确认词不匹配，未执行操作。要求：${expected}"
}

# 用途：生成既有统一部署计划，不执行计划。
# 参数：action为规划动作，component为可选目标。
# 返回值：底层规划器退出码。
plan_deployment() {
  local action="${1:-upgrade}" component="${2:-}"
  case "$action" in install|upgrade|add|repair|resume) ;; *) die "不支持的计划操作：${action}" ;; esac
  if [[ -n "$component" ]]; then
    deploy_run plan --action "$action" --component "$component" --format human
  else
    deploy_run plan --action "$action" --format human
  fi
}

# 用途：展示宿主资源，不隐去读取失败。
# 参数：无。
# 返回值：任一读取失败返回1。
show_host_resources() {
  local failed=0 docker_root
  date -Is || failed=1
  hostname || failed=1
  uname -sm || failed=1
  getconf _NPROCESSORS_ONLN || failed=1
  free -h || failed=1
  df -h "$target_dir" || failed=1
  df -ih "$target_dir" || failed=1
  if docker_root="$(docker info --format '{{.DockerRootDir}}')"; then
    if [[ -n "$docker_root" && -d "$docker_root" ]]; then
      df -h "$docker_root" || failed=1
      df -ih "$docker_root" || failed=1
    else failed=1; fi
  else failed=1; fi
  return "$failed"
}

# 用途：只统计本安装容器，不采集宿主其他应用。
# 参数：无。
# 返回值：Docker统计退出码；无容器返回0。
show_stats() {
  local values ident
  local -a ids=()
  values="$(owned_container_ids)" || return
  while IFS= read -r ident; do [[ -z "$ident" ]] || ids+=("$ident"); done <<<"$values"
  if (("${#ids[@]}" == 0)); then info '本安装没有容器'; return 0; fi
  docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.PIDs}}' "${ids[@]}"
}

# 用途：输出已核对项目范围的容器摘要。
# 参数：无。
# 返回值：Docker查询退出码。
show_containers() {
  owned_container_ids >/dev/null || return
  docker ps -a --filter "label=com.docker.compose.project=$compose_project" \
    --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'
}

# 用途：展示宿主与本安装容器资源并汇总失败。
# 参数：无。
# 返回值：任一读取失败返回1。
show_resources() {
  local failed=0
  show_host_resources || failed=1
  show_stats || failed=1
  return "$failed"
}

# 用途：执行纯元数据概览，不隐式调用底层验收。
# 参数：无。
# 返回值：任一检查失败返回1。
overview() {
  local failed=0
  printf '安装目录：%s\n项目：%s\n' "$target_dir" "$compose_project"
  show_resources || failed=1
  show_containers || failed=1
  ((failed == 0)) || { warn '概览存在读取失败，未宣告通过'; return 1; }
  info '元数据概览完成；未执行部署验收'
}

# 用途：查看组件日志，保留原日志映射和项目归属限制。
# 参数：第一个参数为平台、前端或子系统。
# 返回值：日志命令真实退出码。
show_logs() {
  local target="${1:-}" code values ident owned
  owned="$(owned_container_ids)" || return
  case "$target" in
    platform|frontend)
      local -a services=(frontend)
      [[ "$target" != platform ]] || services=(platform-api platform-worker subsystem-provisioner file-gateway keycloak temporal)
      docker compose --project-name "$compose_project" --project-directory "$target_dir" \
        --file "$compose_file" --env-file "$runtime_file" --env-file "$release_file" \
        logs --tail 200 "${services[@]}"
      ;;
    *)
      validate_subsystem "$target" || return
      code="$(application_code "$target")" || return
      values="$(docker ps -aq --no-trunc --filter "label=com.docker.compose.project=$compose_project" \
        --filter "label=com.basic-platform.application_code=$code")" || return
      [[ -n "$values" ]] || { warn '本安装没有该应用的日志容器'; return 1; }
      while IFS= read -r ident; do
        [[ "$ident" =~ ^[a-f0-9]{64}$ && $'\n'"$owned"$'\n' == *$'\n'"$ident"$'\n'* ]] ||
          die '日志目标不在本安装已核对容器范围内'
        printf '\n===== %s =====\n' "$ident"
        docker logs --tail 200 "$ident" || return
      done <<<"$values"
      ;;
  esac
}

# 用途：记录诊断结果，失败仍保存真实退出码。
# 参数：output为目录，name为固定条目名，其余为采集命令。
# 返回值：实际采集退出码；输出失败返回1。
capture_diagnostic() {
  local output="$1" name="$2" result=0
  shift 2
  {
    printf '采集时间：'; date -Is || result=$?
    if "$@"; then :; else result=$?; fi
    printf '\nEXIT_CODE=%s\n' "$result"
  } >"$output/$name.txt" 2>&1 || return
  printf '%s\t%s\n' "$name" "$result" >>"$output/SUMMARY.tsv" || return
  return "$result"
}

# 用途：独占创建诊断目录，只采集固定元数据清单。
# 参数：第一个参数为可选的输出绝对路径。
# 返回值：任一采集失败返回1，保留已完成的文件。
collect_diagnostics() {
  local output="${1:-/root/uip-diagnostics-$(date -u +%Y%m%dT%H%M%SZ)-$$}" failed=0
  guard_python output "$output" "$target_dir" || return
  mkdir -m 700 -- "$output" || return
  printf 'SECTION\tEXIT_CODE\n' >"$output/SUMMARY.tsv" || return
  capture_diagnostic "$output" host show_host_resources || failed=1
  capture_diagnostic "$output" docker-version docker version || failed=1
  capture_diagnostic "$output" compose-version docker compose version || failed=1
  capture_diagnostic "$output" docker-info docker info --format \
    'Name={{.Name}} ServerVersion={{.ServerVersion}} DockerRootDir={{.DockerRootDir}} CPUs={{.NCPU}} MemTotal={{.MemTotal}}' || failed=1
  capture_diagnostic "$output" containers show_containers || failed=1
  capture_diagnostic "$output" stats show_stats || failed=1
  cat >"$output/README.txt" <<'EOF' || return
本目录仅采集宿主及目标项目元数据，不调用doctor/status/verify，不收集日志或复制配置。
SUMMARY.tsv和各项EXIT_CODE记录真实失败；部分采集失败不表示全部成功。
发送前仍须人工检查主机名、地址、路径、镜像和其他组织敏感信息。
EOF
  info "诊断目录：${output}"
  ((failed == 0)) || { warn '部分诊断采集失败，请查看SUMMARY.tsv'; return 1; }
}

# 用途：确认后执行可能创建一次性容器的底层检查。
# 参数：action为检查命令，component为可选子系统，token为确认词。
# 返回值：底层检查真实退出码。
deployment_check() {
  local action="$1" component="${2:-}" token="${3:-}"
  if [[ "$component" == RUN_DEPLOY_CHECKS ]]; then
    [[ -z "$token" ]] || die '确认词位置不正确，存在多余参数'
    token="$component"; component=''
  fi
  [[ -z "$component" ]] || validate_subsystem "$component" || return
  confirm_exact RUN_DEPLOY_CHECKS "$token" '底层检查可能准备网络、创建一次性验收容器；不等于纯元数据读取。' || return
  if [[ "$action" == status && -z "$component" ]]; then
    show_containers
    return
  fi
  run_exclusive deploy_run "$action" "$component"
}

# 用途：明确确认后创建原有全量备份。
# 参数：第一个参数为CREATE_BACKUP确认词。
# 返回值：备份真实退出码。
create_backup() {
  confirm_exact CREATE_BACKUP "${1:-}" '将创建全量灾备批次，文件网关可能暂时停写。' || return
  run_exclusive deploy_run backup
}

# 用途：校验已有灾备批次，不启动恢复。
# 参数：第一个参数为备份批次绝对目录。
# 返回值：备份校验真实退出码。
verify_backup() {
  guard_python directory "${1:-}" || return
  bash "$backup_tool" --verify-only --backup "$1"
}

# 用途：按当前不可变版本修复，保留底层备份和许可门禁。
# 参数：target为组件，token为精确确认词。
# 返回值：生命周期入口真实退出码。
repair_target() {
  local target="${1:-}" token="${2:-}"
  validate_repair_target "$target" || return
  confirm_exact REPAIR_CURRENT_VERSION "$token" "将重建${target}；保留数据卷，可能备份、停写或迁移。" || return
  run_exclusive lifecycle_run repair "$target"
}

# 用途：执行停用及后置检查，不吞掉任一步骤失败。
# 参数：第一个参数为子系统。
# 返回值：失败步骤的退出码。
disable_steps() {
  deploy_run disable "$1" || return
  deploy_run status "$1"
}

# 用途：明确确认后停用子系统并保留数据。
# 参数：subsystem为子系统，token为确认词。
# 返回值：真实操作退出码。
disable_subsystem() {
  local subsystem="${1:-}"
  validate_subsystem "$subsystem" || return
  confirm_exact DISABLE_KEEP_DATA "${2:-}" "将停用${subsystem}，保留卷、密钥和配置。" || return
  run_exclusive disable_steps "$subsystem"
}

# 用途：恢复后执行验收，前一步失败不继续。
# 参数：第一个参数为子系统。
# 返回值：失败步骤的退出码。
resume_steps() {
  deploy_run resume "$1" || return
  deploy_run verify "$1"
}

# 用途：恢复已接入子系统，保留原有备份、迁移及许可门禁。
# 参数：subsystem为子系统，token为确认词。
# 返回值：真实操作退出码。
resume_subsystem() {
  local subsystem="${1:-}"
  validate_subsystem "$subsystem" || return
  confirm_exact RESUME_SUBSYSTEM "${2:-}" "将恢复${subsystem}；可能备份、迁移并重建服务。" || return
  run_exclusive resume_steps "$subsystem"
}

# 用途：核对状态后继续受控部署，再执行验收。
# 参数：第一个参数为子系统。
# 返回值：任一步骤失败即返回其退出码。
continue_steps() {
  deploy_run status "$1" || return
  deploy_run continue "$1" || return
  deploy_run verify "$1"
}

# 用途：确认平台已采用后继续部署。
# 参数：subsystem为子系统，token为确认词。
# 返回值：真实操作退出码。
continue_subsystem() {
  local subsystem="${1:-}"
  validate_subsystem "$subsystem" || return
  confirm_exact CONTINUE_CONTROLLED_DEPLOYMENT "${2:-}" "确认平台已完成${subsystem}/prod采用及凭据下发后继续。" || return
  run_exclusive continue_steps "$subsystem"
}

# 用途：导入成功后才升级，并报告后置检查失败。
# 参数：component为组件，package为已校验镜像包。
# 返回值：任一步骤失败即返回其退出码。
upgrade_steps() {
  local component="$1" package="$2"
  guard_python package "$package" "$component" || return
  deploy_run import "$package" || return
  deploy_run upgrade "$component" || return
  if [[ "$component" == platform || "$component" == frontend ]]; then
    deploy_run verify || return
    info '核心组件发布及底层验收完成；不代表全部业务或外部访问已验收'
  else
    deploy_run status "$component" || return
    info '业务镜像已暂存；请在平台prod环境受控更新，再执行continue与verify'
  fi
}

# 用途：区分核心实际发布和业务暂存，先核对包组件。
# 参数：component为组件，package为镜像包，token为精确确认词。
# 返回值：真实操作退出码。
stage_upgrade() {
  local component="${1:-}" package="${2:-}" token="${3:-}" expected=STAGE_UPGRADE
  validate_upgrade_component "$component" || return
  [[ -n "$package" ]] || die 'upgrade必须指定新镜像包'
  if [[ "$package" != /* ]]; then
    package="$(python3 -c 'from pathlib import Path; import sys; print(Path.cwd()/sys.argv[1])' "$package")" || return
  fi
  guard_python package "$package" "$component" || return
  if [[ "$component" == platform || "$component" == frontend ]]; then
    expected=PUBLISH_CORE_UPGRADE
    warn '核心upgrade会实际发布，可能执行备份、迁移与服务重建'
  fi
  confirm_exact "$expected" "$token" "将处理${component}的新镜像包；已完成步骤不会自动回滚。" || return
  run_exclusive upgrade_steps "$component" "$package"
}

# 用途：校验CLI参数数量，防止多余参数被忽略。
# 参数：command为命令，其余为用户参数。
# 返回值：合法返回0，否则退出1。
validate_arguments() {
  local command="$1" count min=0 max=0
  shift; count=$#
  case "$command" in
    menu|overview|resources) ;;
    logs|verify-backup) min=1; max=1 ;;
    collect|backup) max=1 ;;
    plan|doctor|status|verify) max=2 ;;
    repair|disable|resume|continue) min=1; max=2 ;;
    upgrade) min=2; max=3 ;;
    *) die "未知命令：${command}" ;;
  esac
  ((count >= min && count <= max)) || die "命令${command}的参数数量不正确"
}

# 用途：按固定白名单调度命令。
# 参数：command为命令，其余为已校验参数。
# 返回值：对应操作真实退出码。
dispatch() {
  local command="$1"
  shift
  case "$command" in
    menu) interactive_menu ;;
    overview) overview ;;
    resources) show_resources ;;
    logs) show_logs "${1:-}" ;;
    collect) collect_diagnostics "${1:-}" ;;
    plan) plan_deployment "${1:-upgrade}" "${2:-}" ;;
    doctor|status|verify) deployment_check "$command" "${1:-}" "${2:-}" ;;
    backup) create_backup "${1:-}" ;;
    verify-backup) verify_backup "${1:-}" ;;
    repair) repair_target "${1:-}" "${2:-}" ;;
    disable) disable_subsystem "${1:-}" "${2:-}" ;;
    resume) resume_subsystem "${1:-}" "${2:-}" ;;
    continue) continue_subsystem "${1:-}" "${2:-}" ;;
    upgrade) stage_upgrade "${1:-}" "${2:-}" "${3:-}" ;;
  esac
}

# 用途：从菜单选择业务子系统。
# 参数：无，从当前输入读取。
# 返回值：子系统参数；无效或EOF返回1。
select_subsystem() {
  local choice
  printf '1.客户与商机 2.客户门户 3.合同 4.项目 5.结算 6.数据分析\n' >&2
  IFS= read -r choice || return 1
  case "$choice" in
    1) printf customer-opportunity ;; 2) printf customer-portal ;;
    3) printf contract ;; 4) printf project ;;
    5) printf settlement ;; 6) printf data-analysis ;; *) return 1 ;;
  esac
}

# 用途：执行一次菜单选择，变更确认由操作函数负责。
# 参数：第一个参数为菜单编号。
# 返回值：操作退出码；输入EOF返回1。
menu_action() {
  local subsystem target package output
  case "$1" in
    1) overview ;;
    2|3|4)
      printf '子系统（留空全部/基础平台）：'
      IFS= read -r subsystem || return
      case "$1" in 2) target=doctor ;; 3) target=status ;; 4) target=verify ;; esac
      deployment_check "$target" "$subsystem"
      ;;
    5) printf '日志目标：'; IFS= read -r target || return; show_logs "$target" ;;
    6) show_resources ;;
    7) printf '诊断输出绝对目录（可留空）：'; IFS= read -r output || return; collect_diagnostics "$output" ;;
    8) create_backup ;;
    9) printf '备份批次绝对目录：'; IFS= read -r output || return; verify_backup "$output" ;;
    10) printf '修复目标：'; IFS= read -r target || return; repair_target "$target" ;;
    11|12|13)
      subsystem="$(select_subsystem)" || return
      case "$1" in
        11) disable_subsystem "$subsystem" ;;
        12) resume_subsystem "$subsystem" ;;
        13) continue_subsystem "$subsystem" ;;
      esac
      ;;
    14)
      printf '升级组件：'; IFS= read -r target || return
      printf '镜像包绝对路径：'; IFS= read -r package || return
      stage_upgrade "$target" "$package"
      ;;
    15)
      printf '计划操作（默认upgrade）：'; IFS= read -r target || return
      printf '组件（可留空）：'; IFS= read -r package || return
      plan_deployment "${target:-upgrade}" "$package"
      ;;
    *) warn '选择无效'; return 1 ;;
  esac
}

# 用途：循环菜单，失败明确提示，EOF正常退出。
# 参数：无。
# 返回值：退出菜单返回0。
interactive_menu() {
  local choice result
  while true; do
    load_installation || return
    printf '\n========== 统一身份认证平台运维 ==========\n'
    printf '目标：%s  项目：%s\n' "$target_dir" "$compose_project"
    printf '1.只读概览 2.doctor检查 3.status检查 4.verify验收\n'
    printf '5.日志 6.资源 7.诊断采集 8.全量备份 9.校验备份\n'
    printf '10.修复 11.停用 12.恢复 13.继续受控部署 14.升级 15.只读计划 0.退出\n'
    IFS= read -r choice || return 0
    [[ "$choice" != 0 ]] || return 0
    if (menu_action "$choice"); then :; else
      result=$?
      warn "本次操作失败/取消（退出码${result}）；已完成步骤不会自动撤销"
    fi
  done
}

# 用途：解析命令，校验服务器、安装及Docker范围后执行。
# 参数：命令行原始参数。
# 返回值：命令真实退出码。
main() {
  local command=menu seen_target=false
  while (($#)); do
    case "$1" in
      --target)
        (($# >= 2)) || die '--target缺少参数'
        [[ "$seen_target" == false ]] || die '--target不能重复'
        target_dir="$2"; seen_target=true; shift 2 ;;
      --help|-h|help) usage; return 0 ;;
      --) shift; break ;;
      -*) die "未知选项：$1" ;;
      *) break ;;
    esac
  done
  if (($#)); then command="$1"; shift; fi
  validate_arguments "$command" "$@" || return
  require_server || return
  need python3 || return
  load_installation || return
  if [[ "$command" == verify-backup ]]; then
    dispatch "$command" "$@"
    return
  fi
  if [[ "$command" == collect ]]; then
    if command -v docker >/dev/null 2>&1; then require_local_docker || return; fi
    dispatch "$command" "$@"
    return
  fi
  need docker || return
  require_local_docker || return
  owned_container_ids >/dev/null || return
  if [[ "$command" == menu && ! -t 0 ]]; then die '非交互执行必须指定命令'; fi
  dispatch "$command" "$@"
}

main "$@"
