#!/usr/bin/env python3
"""审计改造批次 E 的只读验收助手。

只做三件事：
  1. 检查本地网关与各系统探活（只读 HTTP）；
  2. 打印本机**已具备**的审计相关端点，供有权限的验收人手工执行；
  3. 明确标注哪些步骤需要授权（写操作、迁移、部署），不会自己执行。

不读数据库、不写业务数据、不调用需要管理员令牌的接口。
"""
import json
import sys
import urllib.error
import urllib.request

GATEWAY = "http://127.0.0.1:8081"
CHECKS = [
    ("基础平台网关", "/healthz"),
    ("数据分析", "/data_analysis/healthz"),
    ("合同管理（未登录应 401）", "/contract_management/api/v1/auth/me"),
    ("项目管理系统（未登录应 401/403）", "/project_management/api/v1/projects"),
    ("结算系统（未登录应 401）", "/settlement/api/v1/auth/me"),
    ("客户与商机（未登录应 401）", "/customer-opportunity/api/v1/auth/me"),
]

MANUAL_STEPS = [
    ("平台", "GET /api/v1/audit/ingestion-health", "摄取健康：最近一次成功投递、积压数量"),
    ("平台", "GET /api/v1/audit/events?page=1&page_size=20&actor_id=<用户ID>", "按稳定操作者 ID 检索"),
    ("平台", "GET /api/v1/audit/events?page=1&page_size=20&correlation_id=<关联ID>", "按业务关联检索"),
    ("平台", "GET /api/v1/audit/events?page=1&page_size=20&login_ip=<登录IP>", "按登录地址检索"),
    ("合同", "创建→提交→审批→下载 docx", "对应 CREATED / APPROVAL_* / DOCUMENT_DOWNLOADED"),
    ("结算", "确认应收→收款→分摊→导出报表", "对应 RECEIVABLE_PLAN_CONFIRMED / RECEIPT_RECORDED / ALLOCATION_CONFIRMED / REPORT_EXPORT_DOWNLOADED"),
    ("项目", "建项→交付", "对应 PROJECT_CREATED / DELIVERED"),
    ("门户", "导出项目报告并下载", "对应 PORTAL_DOWNLOAD_GRANT_ISSUED / _CONSUMED"),
]

NEEDS_AUTHORIZATION = [
    "启动或重启任何容器（含本地栈）",
    "执行数据库迁移（含 audit_event.login_ip_source、prj_audit_outbox.actor_type）",
    "部署到服务器或下发配置（APP_TRUSTED_PROXIES 等）",
    "执行归档/清理任务（会删除在线记录，需归档校验通过）",
]


def fetch(path):
    try:
        with urllib.request.urlopen(GATEWAY + path, timeout=6) as response:
            return response.status, ""
    except urllib.error.HTTPError as error:
        return error.code, ""
    except (urllib.error.URLError, TimeoutError) as error:
        return 0, str(error)


def main():
    failures = 0
    print("[E] 只读探活")
    for name, path in CHECKS:
        status, error = fetch(path)
        if status == 0:
            failures += 1
            print("  ❌ %-28s %s 不可达（%s）" % (name, path, error))
            continue
        print("  ✅ %-28s %s -> %s" % (name, path, status))

    print("\n[E] 需要登录后手工执行的核对项（脚本不代执行）")
    for owner, call, expectation in MANUAL_STEPS:
        print("  · %-4s %s\n      期望：%s" % (owner, call, expectation))

    print("\n[E] 需要老大明确授权才能执行的步骤")
    for item in NEEDS_AUTHORIZATION:
        print("  ⚠ " + item)

    print("\n[E] 结论")
    if failures:
        print("  %d 个端点不可达：环境未就绪，不能宣称通过全链验收。" % failures)
        return 1
    print("  端点全部可达；手工核对项与授权项完成前，仍不构成“全链验收通过”。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
