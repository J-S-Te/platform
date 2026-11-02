#!/usr/bin/env python3
"""在已签客户目录外层生成入口；不修改镜像、归档或发布证明。"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import sys

SPEC = importlib.util.spec_from_file_location(
    "customer_install_generation_runtime", Path(__file__).with_name("customer-install.py"))
RUNTIME = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNTIME)

ENTRY = '''#!/usr/bin/env bash
# 客户入口：只编排外层准备，实际安装执行独立验签后的原部署脚本。
set -Eeuo pipefail
delivery_dir="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# 用途：在执行辅助程序前核对固定摘要，拒绝链接或被替换的工具。
# 参数：第一个参数为相对路径，第二个参数为固定 SHA-256。
# 返回值：一致返回零；工具缺失、摘要不符或不安全时退出。
check_bound_file() {
  local relative="$1" expected="$2" actual
  [[ ! -L "$delivery_dir/工具" && -f "$delivery_dir/$relative" && ! -L "$delivery_dir/$relative" ]] || {
    printf '[停止] 工具缺失或路径不安全：%s\\n' "$relative" >&2; exit 1;
  }
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$delivery_dir/$relative")"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$delivery_dir/$relative")"
  else
    printf '%s\\n' '[停止] 缺少 SHA-256 工具，请管理员准备 sha256sum。' >&2; exit 1
  fi
  [[ "${actual%% *}" == "$expected" ]] || {
    printf '[停止] 工具摘要不符：%s；禁止执行。\\n' "$relative" >&2; exit 1;
  }
}
check_bound_file 工具/customer-install.py @RUNTIME@
check_bound_file 工具/customer-install.json @CONFIG@
command -v python3 >/dev/null 2>&1 || {
  printf '%s\\n' '[停止] 缺少 Python 3.10+，请管理员准备后重试。' >&2; exit 1;
}
exec python3 "$delivery_dir/工具/customer-install.py" "$@"
'''


# 用途：生成不进入已签介质的客户入口和固定批次配置。
# 参数：delivery 是已有客户交付目录；只新增外层文件并补齐工具校验清单。
# 返回值：批次配置；存在不一致的入口或材料时拒绝覆盖。
def prepare(delivery):
    delivery = RUNTIME.safe_path(delivery)
    for name in ("镜像", "工具", "公钥", "交付清单"):
        if not RUNTIME.safe_path(delivery / name).is_dir():
            raise RUNTIME.InstallError("客户目录缺少：" + name)
    archives = list((delivery / "镜像").glob("*.tar"))
    if len(archives) != 1:
        raise RUNTIME.InstallError("客户目录必须包含唯一总归档")
    archive = archives[0]
    facts = RUNTIME.archive_info(archive)
    manifest = facts["manifest"]
    config = {"schema": "urn:uip:customer-install:v1", "customer": delivery.name,
              "release_id": manifest.get("release_id"),
              "target_platform": manifest.get("target_platform"),
              "archive": archive.name, "archive_size": archive.stat().st_size,
              "archive_sha256": RUNTIME.sha256(archive), "archive_root": facts["root"],
              "manifest_sha256": facts["manifest_sha256"], "delivery_id": facts["delivery_id"],
              "trust_sha256": RUNTIME.sha256(delivery / "公钥/trust-policy.json"),
              "verifier_sha256": RUNTIME.sha256(delivery / "工具/release-verifier-linux-amd64"),
              "installer_sha256": RUNTIME.sha256(delivery / "工具/安装发布信任.sh")}
    RUNTIME.inspect_delivery(delivery, config)
    runtime = RUNTIME.read_file(Path(__file__).with_name("customer-install.py"))
    encoded = (json.dumps(config, ensure_ascii=False, indent=2, sort_keys=True) + "\n").encode()
    entry = ENTRY.replace("@RUNTIME@", hashlib.sha256(runtime).hexdigest()).replace(
        "@CONFIG@", hashlib.sha256(encoded).hexdigest()).encode()
    files = {"工具/customer-install.py": runtime, "工具/customer-install.json": encoded,
             "一键安装.sh": entry, "开始安装.md": instructions(config, hashlib.sha256(entry).hexdigest())}
    publish_files(delivery, files)
    update_tool_checksums(delivery, files)
    return config


# 用途：批量发布固定内容，已有内容必须一致，避免覆盖未知客户文件。
# 参数：delivery 是客户根目录，files 是相对文件名到字节的映射。
# 返回值：无；冲突发生在新增之前，原文件保持不变。
def publish_files(delivery, files):
    for name, content in files.items():
        path = RUNTIME.safe_path(delivery / name)
        if path.exists() and RUNTIME.read_file(path) != content:
            raise RUNTIME.InstallError("已有文件与新入口不同，拒绝覆盖：" + name)
    for name, content in files.items():
        path = delivery / name
        if not path.exists():
            with path.open("xb") as stream:
                stream.write(content)
            path.chmod(0o755 if name.endswith(".sh") else 0o644)


# 用途：在保留既有工具绑定的前提下补充新辅助工具摘要。
# 参数：delivery 是客户目录，files 是本次生成文件。
# 返回值：无；原工具摘要错误或校验清单不安全时阻断。
def update_tool_checksums(delivery, files):
    path = RUNTIME.safe_path(delivery / "工具/SHA256SUMS")
    old = RUNTIME.read_file(path).decode()
    entries = {}
    for line in old.splitlines():
        parts = line.split()
        if len(parts) != 2 or not RUNTIME.DIGEST.fullmatch(parts[0]):
            raise RUNTIME.InstallError("既有工具校验清单无效")
        name = parts[1]
        if Path(name).name != name or name in entries:
            raise RUNTIME.InstallError("既有工具校验路径重复或不安全")
        if RUNTIME.sha256(delivery / "工具" / name) != parts[0]:
            raise RUNTIME.InstallError("既有工具摘要不符，禁止重算掩盖：" + name)
        entries[name] = parts[0]
    for name in ("release-verifier-linux-amd64", "安装发布信任.sh", "customer-install.py", "customer-install.json"):
        entries[name] = RUNTIME.sha256(delivery / "工具" / name)
    content = "".join(digest + "  " + name + "\n" for name, digest in sorted(entries.items()))
    if old != content:
        temporary = path.with_name("SHA256SUMS.oneclick-new")
        with temporary.open("x") as stream:
            stream.write(content)
        temporary.chmod(0o644)
        os.replace(temporary, path)


# 用途：生成客户只需阅读的短说明与独立核对信息。
# 参数：config 是固定批次，entry_digest 是外层入口摘要。
# 返回值：Markdown UTF-8 字节。
def instructions(config, entry_digest):
    return f'''# {config["customer"]}：从这里开始安装

本目录对应 `{config["release_id"]}`，适用于 Linux/amd64。请上传**整个目录**，不要只上传镜像。
首次运行前，请通过厂商独立可信渠道核对本入口及下方信任摘要；同包摘要只能发现传输损坏。

```bash
cd /srv/北京  # 替换为实际上传的客户目录
sudo bash ./一键安装.sh
```

入口自动检查固定批次和文件摘要，独立验签后调用**本批已签归档内原版**
`服务器一键离线部署.sh`，自动预检并首装。默认安装目录 `/opt/unified-identity-platform`。
按照原向导填写服务器地址等必要配置；初始凭据由原部署器按保护规则保存，不写入本交付目录。
需要管理员事先准备 Docker Engine、Compose、Bash 4.3+、Python 3.10+；其余随包宿主依赖
（jq、curl、openssl、flock 等）由入口按目标机 Ubuntu 版本匹配本批离线依赖包自动安装，不联网下载。

如显示“待处理”，基础部署与全部业务可用是不同状态：在基础平台的“许可证”页面导出实例申请，
交正式签发方签发，再导入返回的正式许可证；在应用管理中完成各系统的受控接入。
客户实例 ID、customer_id、商业 JWS、运行批准和自然 ACK 不能由此入口伪造，目录名不能代替客户 ID。
保留原部署器给出的具体待办；完成后**再次运行同一条命令**，自动重新验签并按原会话续跑。
退出码 `2` 表示尚有真实人工待办，`1` 表示门禁阻断或执行失败，`0` 才表示该命令对应的阶段完成。

按 Ctrl+C 可请求停止，入口等候原进程退出并保留会话；不会自动回滚、删除数据库或重新构建镜像。
异常断电后也重新运行本入口；若目标或会话不匹配会阻断，交管理员使用原恢复流程，不能删除记录硬重装。
不要同时使用其他部署命令操作同一安装目标；原部署器的安装锁仍保留。

只检查收到的材料（不执行验签、不安装）：`bash ./一键安装.sh --inspect`。
宿主信任已安装后，仅检查：`sudo bash ./一键安装.sh --check-only`（只读，不安装宿主依赖；缺依赖时原预检会明确提示未执行资产预检，正式安装会自动补齐）。
首次安装执行原首装预检；已有会话则只做独立验签和会话身份/状态核验，不能据此宣布恢复就绪。
高级选项：`--target /绝对安装路径`、`--cache-dir /管理员专属缓存`；首次选定后续跑应保持相同。
缓存默认 `/var/lib/uip-customer-install`，保存原介质解包副本和安装关联记录；应保留以便续跑。
已有其他部署时入口不会自动升级；不同信任材料不会自动覆盖。

## 交给厂商独立核对的摘要

- 一键安装.sh SHA-256：`{entry_digest}`
- trust-policy.json SHA-256：`{config["trust_sha256"]}`
- release-verifier SHA-256：`{config["verifier_sha256"]}`
- 已签总归档 SHA-256：`{config["archive_sha256"]}`

新入口位于签名归档外；旧镜像、归档、发布清单和签名均保持原样。
此入口不负责签发许可证，不自动采用应用，也不将人工待办算成安装完成。
'''.encode()


# 用途：供厂商交付整理流程或既有客户目录补齐工具使用。
# 参数：无，读取命令行 --delivery。
# 返回值：无；失败退出非零且不重建镜像。
def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--delivery", required=True)
    args = parser.parse_args()
    config = prepare(args.delivery)
    print("[信息] 客户外层一键安装入口已准备：" + config["customer"] + "/" + config["release_id"])


if __name__ == "__main__":
    try:
        main()
    except (RUNTIME.InstallError, OSError, ValueError) as error:
        print("[停止] " + str(error), file=sys.stderr)
        sys.exit(1)
