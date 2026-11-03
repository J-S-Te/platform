#!/usr/bin/env python3
"""Project the unified Compose file for a delivery component selection.

The repository Compose file is intentionally one file with subsystem markers.
For a delivery that excludes a subsystem we comment its service block and drop
top-level volumes that are referenced only by the disabled block. This keeps
the packaged asset from materialising volumes for services that have no image
or runtime profile in that delivery.
"""
from __future__ import annotations

import argparse
from pathlib import Path
import re
import sys

BLOCK_TEMPLATE = r"(?ms)^  # BEGIN SUBSYSTEM: {name} .*?^  # END SUBSYSTEM: {name} [^\n]*"
# 子系统标记行必须保持可解析：升级脚本按 `^\s*#\s*(BEGIN|END)\s+SUBSYSTEM\s*[:：]`
# 推导块状态，标记行一旦被再次注释（`#   # BEGIN ...`）就整块不可见，受控升级会误判
# "未启用或缺少模块"。空行与块内既有注释同样无需二次注释。
SUBSYSTEM_MARKER = re.compile(r"^[ \t]*#[ \t]*(BEGIN|END)[ \t]+SUBSYSTEM[ \t]*[:：]")
COMMENTED_LINE = re.compile(r"^[ \t]*#")


def comment_lines(lines: list[str]) -> str:
    """注释块内容但保留标记行与既有注释，保证块状态仍可被部署脚本解析。"""
    commented = []
    for line in lines:
        if SUBSYSTEM_MARKER.match(line) or not line.strip() or COMMENTED_LINE.match(line):
            commented.append(line)
        else:
            commented.append("# " + line)
    return "\n".join(commented)
TOP_VOLUME = re.compile(r"^  ([A-Za-z0-9_.-]+):\s*$")
MOUNT_REF = re.compile(r"^\s*-\s+([A-Za-z0-9_.-]+):")
TOP_LEVEL = re.compile(r"^[A-Za-z0-9_.-]+:")
VOLUMES_HEADER = re.compile(r"^volumes:\s*$")


class ProjectionError(ValueError):
    pass


def block_match(text: str, name: str):
    return re.search(BLOCK_TEMPLATE.format(name=re.escape(name)), text)


def comment_block(text: str, name: str) -> str:
    pattern = BLOCK_TEMPLATE.format(name=re.escape(name))
    result, count = re.subn(pattern, lambda m: comment_lines(m[0].splitlines()), text)
    if count != 1:
        raise ProjectionError(f"missing or duplicate subsystem block: {name}")
    return result


def volume_ranges(text: str) -> dict[str, tuple[int, int]]:
    lines = text.splitlines(keepends=True)
    try:
        header = next(i for i, line in enumerate(lines) if VOLUMES_HEADER.match(line))
    except StopIteration as exc:
        raise ProjectionError("compose has no top-level volumes section") from exc
    ranges: dict[str, tuple[int, int]] = {}
    current = None
    for index in range(header + 1, len(lines)):
        line = lines[index]
        if line and not line.startswith((" ", "\t")):
            if current is not None:
                ranges[current][1] = index
            break
        match = TOP_VOLUME.match(line)
        if match:
            if current is not None:
                ranges[current][1] = index
            current = match.group(1)
            ranges[current] = [index, len(lines)]
        elif current is not None:
            ranges[current][1] = index + 1
    return {name: (start, end) for name, (start, end) in ranges.items()}


def mount_refs(text: str, defined: set[str]) -> set[str]:
    refs = set()
    for line in text.splitlines():
        match = MOUNT_REF.match(line)
        if match and match.group(1) in defined:
            refs.add(match.group(1))
    return refs


def project(text: str, disabled: list[str]) -> str:
    if not disabled:
        return text
    defined = set(volume_ranges(text))
    disabled_refs: set[str] = set()
    output = text
    for name in disabled:
        match = block_match(output, name)
        if match is None:
            raise ProjectionError(f"missing or duplicate subsystem block: {name}")
        disabled_refs |= mount_refs(match.group(0), defined)
        output = comment_block(output, name)
    # A volume may be shared with an enabled subsystem; only remove volumes that
    # no longer have any enabled service reference.
    enabled_refs = mount_refs(output, defined)
    remove = disabled_refs - enabled_refs
    if remove:
        ranges = volume_ranges(output)
        lines = output.splitlines(keepends=True)
        for volume in sorted(remove, key=lambda key: ranges[key][0], reverse=True):
            start, end = ranges[volume]
            del lines[start:end]
        output = "".join(lines)
    return output


def block_states(text: str) -> dict[str, str]:
    """按部署脚本同款规则推导块状态：块内存在任何未注释的非空行即 enabled。"""
    states: dict[str, str] = {}
    key = ""
    content = 0
    for line in text.splitlines():
        begin = re.match(r"^[ \t]*#[ \t]*BEGIN[ \t]+SUBSYSTEM[ \t]*[:：][ \t]*([a-z][a-z0-9-]*)", line)
        if begin:
            if key:
                raise ProjectionError(f"nested subsystem block: {line.strip()}")
            key, content = begin.group(1), 0
            continue
        end = re.match(r"^[ \t]*#[ \t]*END[ \t]+SUBSYSTEM[ \t]*[:：][ \t]*([a-z][a-z0-9-]*)", line)
        if end:
            if key != end.group(1):
                raise ProjectionError(f"BEGIN/END mismatch: {line.strip()}")
            states[key] = "enabled" if content else "disabled"
            key = ""
            continue
        if key and line.strip() and not COMMENTED_LINE.match(line):
            content += 1
    if key:
        raise ProjectionError("unclosed subsystem block")
    return states


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--disabled", default="", help="comma-separated subsystem markers")
    parser.add_argument("--require-enabled", default="",
                        help="comma-separated markers that must stay enabled after projection")
    args = parser.parse_args()
    source = Path(args.input)
    destination = Path(args.output)
    if source.is_symlink() or not source.is_file():
        print("compose input is not a regular file", file=sys.stderr)
        return 1
    disabled = [value.strip() for value in args.disabled.split(",") if value.strip()]
    required = [value.strip() for value in args.require_enabled.split(",") if value.strip()]
    try:
        projected = project(source.read_text(encoding="utf-8"), disabled)
        states = block_states(projected)
        if states.get("common") != "enabled":
            raise ProjectionError("common subsystem block must stay enabled")
        for name in disabled:
            if states.get(name) != "disabled":
                raise ProjectionError(f"subsystem block must be fully commented: {name}")
        for name in required:
            if states.get(name) != "enabled":
                raise ProjectionError(f"subsystem block disabled but shipped in this batch: {name}")
    except (OSError, UnicodeError, ProjectionError) as exc:
        print(f"compose projection failed: {exc}", file=sys.stderr)
        return 1
    destination.write_text(projected, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
