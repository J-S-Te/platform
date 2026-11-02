#!/usr/bin/env python3
"""打包后自检：部署资产内脚本引用的运行时文件必须随包交付。

背景：`server_oneclick` 的 deployment-assets 使用显式白名单 `asset_files`，
新增运行时辅助脚本时容易漏项，问题只会在安装到对应阶段（例如平台发布执行
`ls bin/initialize-audit-hash-key.py`）时才暴露。此检查在归档前阻断：
扫描已暂存资产中的每个 shell 脚本，凡以 `$script_dir/`、`$<var>/bin/` 或
`bash bin/` 形式引用、且在构建机 `bin/` 中真实存在的文件，都必须出现在资产包里。
"""
import argparse
from pathlib import Path
import re
import sys

SELF_REFERENCE = re.compile(r"\$\{?(?:script_dir|bin_dir)\}?/([A-Za-z0-9._-]+\.(?:sh|py))")
BIN_REFERENCE = re.compile(r"\$\{?[A-Za-z_][A-Za-z0-9_]*\}?/bin/([A-Za-z0-9._-]+\.(?:sh|py))")
INVOKE_REFERENCE = re.compile(
    r"(?:^|[;&|(\s])(?:bash|sh|python3?(?:\s+-[A-Za-z-]+)*|source|\.)\s+[\"']?bin/([A-Za-z0-9._-]+\.(?:sh|py))",
    re.MULTILINE,
)


# 用途：读取本次待打包的文件清单。
# 参数：stage 或 manifest 二选一，stage 为已暂存目录。
# 返回值：相对资产根的路径集合；输入不可读时抛错。
def bundle_files(stage, manifest):
    if stage is not None:
        root = Path(stage)
        if not root.is_dir():
            raise ValueError("暂存目录不存在：" + str(root))
        return {item.relative_to(root).as_posix() for item in root.rglob("*") if item.is_file()}
    lines = Path(manifest).read_text(encoding="utf-8").splitlines()
    return {line.strip() for line in lines if line.strip() and not line.lstrip().startswith("#")}


# 用途：扫描资产内 shell 脚本，找出随包缺失的运行时引用。
# 参数：files 是资产路径集合，script_root 是脚本实际内容所在目录，source_bin 是 bin 源目录。
# 返回值：缺失项列表，元素为（引用方，缺失文件）元组；只报告源目录真实存在的文件。
def missing_references(files, script_root, source_bin):
    missing = []
    for name in sorted(files):
        if not name.endswith(".sh"):
            continue
        script = script_root / name
        if not script.is_file():
            continue
        text = script.read_text(encoding="utf-8", errors="replace")
        referenced = set(SELF_REFERENCE.findall(text)) | set(BIN_REFERENCE.findall(text)) \
            | set(INVOKE_REFERENCE.findall(text))
        for helper in sorted(referenced):
            packaged = "bin/" + helper
            if packaged in files or not (source_bin / helper).is_file():
                continue
            missing.append((name, packaged))
    return missing


# 用途：提供打包门禁命令行。
# 参数：无，读取 --stage/--manifest、--source-root、--source-bin。
# 返回值：无缺失返回 0，否则非零并列出引用链。
def main():
    parser = argparse.ArgumentParser(description="检查部署资产是否遗漏被脚本引用的运行时文件")
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--stage", help="已暂存的部署资产目录")
    source.add_argument("--manifest", help="部署资产相对路径清单，每行一个")
    parser.add_argument("--source-root", help="构建机部署目录（默认 source-bin 的父目录）")
    parser.add_argument("--source-bin", required=True, help="构建机 bin 源目录")
    args = parser.parse_args()
    source_bin = Path(args.source_bin)
    source_root = Path(args.source_root) if args.source_root else source_bin.parent
    try:
        files = bundle_files(args.stage, args.manifest)
        script_root = Path(args.stage) if args.stage else source_root
        missing = missing_references(files, script_root, source_bin)
    except (OSError, ValueError) as error:
        print("[错误] " + str(error), file=sys.stderr)
        raise SystemExit(1)
    if missing:
        for script, helper in missing:
            print("[缺失] %s 引用了未随包交付的 %s" % (script, helper), file=sys.stderr)
        print("[错误] 部署资产缺少被引用的运行时文件，拒绝打包；请补入 asset_files 白名单", file=sys.stderr)
        raise SystemExit(1)
    print("[信息] 部署资产运行时引用完整，共检查 %d 个文件" % len(files))
    return 0


if __name__ == "__main__":
    sys.exit(main())