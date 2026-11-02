#!/usr/bin/env python3
"""校验两份生产 Compose 的一次性任务服务（contract-migrate / contract-number-audit）配置一致。

生产部署真入口是 docker-compose.yml（bin/deploy-service.sh 直接读取它），compose.yaml 是
「不随资产包发布」的参考副本。此前 contract-number-audit 是手工加进两份文件的，没有任何测试
保证下次两边同步，实际已经出现过「只改一份、任务前提指错文件」的事故。本用例把这份一致性
固化成回归守卫：服务存在性、contract-number-audit 的关键键、以及它与 contract-migrate 的
逐键一致性（image / env_file 有效引用 / depends_on）。

解析方式：本机与 CI 的 python3 都没有 pyyaml，而 docker-compose.yml 带 YAML anchor 与 x- 扩展段、
compose.yaml 用 `<<: *contract-service` 合并键，纯文本/正则解析无法还原 merge 结果（compose.yaml 的
contract-number-audit 根本没有重复声明 image/env_file）。因此沿用仓库既有做法调用 ruby
（见 tests/test-compose-blocks.py）：ruby -ryaml 解析成 JSON，Python 侧只做断言，不引入新依赖。
注：`HTTP_ADDRESS: :8081` 这类标量会被 Psych 扫成 Symbol，所以显式允许 Symbol。

本用例只在 CI/开发机跑，不进离线资产包：compose.yaml 是不随资产包发布的参考副本
（bin/build-offline-packages.sh 的静态资产清单只收 docker-compose.yml），缺了它本用例
必然失败，因此不要把本文件加进那份清单。
"""
from pathlib import Path
import json
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[1]
# 生产真入口在前，参考副本在后；两边的关键键必须一致。
CANONICAL = "docker-compose.yml"
REFERENCE = "compose.yaml"
COMPOSE_FILES = (CANONICAL, REFERENCE)
ONE_SHOT_SERVICES = ("contract-migrate", "contract-number-audit")
MIGRATE_SERVICE = "contract-migrate"
AUDIT_SERVICE = "contract-number-audit"
MYSQL_DEPENDENCY = "contract-mysql"

RUBY_LOADER = (
    'require "json"; require "yaml"; '
    "doc = YAML.safe_load(File.read(ARGV[0]), permitted_classes: [Symbol], aliases: true); "
    "puts JSON.generate(doc)"
)


def load_compose(filename):
    """用 ruby -ryaml 加载一份 Compose 文件，返回还原了 anchor/merge 的字典。

    参数：filename 为相对 deploy/production 的文件名。
    返回值：解析后的字典；解析失败时抛出带文件名的中文断言错误。
    """
    path = ROOT / filename
    result = subprocess.run(
        ["ruby", "-e", RUBY_LOADER, str(path)], capture_output=True, text=True, check=False
    )
    if result.returncode != 0:
        raise AssertionError(
            f"无法解析 {filename}：ruby -ryaml 返回 {result.returncode}，stderr={result.stderr.strip()!r}"
        )
    return json.loads(result.stdout)


def describe(filename, service, key, expected, actual):
    """生成中文化的失败信息：哪份文件、哪个服务、哪个键、期望什么、实际什么。"""
    return (
        f"{filename} 的 {service} 服务键 {key} 不符合约定："
        f"期望 {expected!r}，实际 {actual!r}"
    )


def env_file_references(service):
    """归一 env_file 的两种写法，返回有效引用路径列表。

    docker-compose.yml 用 `- path: ... / required: false` 的映射写法，compose.yaml 用
    简化字符串写法；两种写法引用同一批运行配置文件，否则巡检会读到与迁移不同的
    MYSQL_DSN 来源（两个一次性任务的服务配置就漂移了）。

    参数：service 为单个服务的解析结果。
    返回值：按声明顺序的有效引用路径；某条目取不到路径时抛中文断言错误。
    """
    entries = service.get("env_file") or []
    if isinstance(entries, str):
        entries = [entries]
    references = []
    for entry in entries:
        path = entry.get("path") if isinstance(entry, dict) else entry
        if not isinstance(path, str) or not path.strip():
            raise AssertionError(f"env_file 条目无法解析出有效引用路径：{entry!r}")
        references.append(path)
    return references


class OneShotComposeParityTest(unittest.TestCase):
    """两份生产 Compose 的一次性任务服务必须逐键对齐。"""

    @classmethod
    def setUpClass(cls):
        cls.compose = {filename: load_compose(filename) for filename in COMPOSE_FILES}

    def services(self, filename):
        """返回某份文件的 services 映射；缺失时直接判失败。"""
        services = self.compose[filename].get("services")
        self.assertIsInstance(
            services, dict, f"{filename} 缺少顶层 services 映射：实际 {services!r}"
        )
        return services

    def service(self, filename, name):
        """返回指定服务；不存在时给出中文失败信息。

        刻意不用 assertIn：unittest 默认 longMessage 会把整个 services 字典塞进报错里，
        运维只需要看到「哪份文件、哪个服务、现有哪些合同服务」。
        """
        services = self.services(filename)
        if name not in services:
            contract_services = sorted(key for key in services if key.startswith("contract"))
            self.fail(
                f"{filename} 缺少一次性任务服务 {name}："
                f"实际存在的合同服务为 {contract_services}"
                f"（生产真入口是 {CANONICAL}，两份 Compose 已漂移）"
            )
        return services[name]

    def test_one_shot_services_exist_in_both_compose_files(self):
        # 两份文件都必须登记同组一次性任务；少一份运行时就会「同一发布两套任务」。
        for filename in COMPOSE_FILES:
            for name in ONE_SHOT_SERVICES:
                with self.subTest(filename=filename, service=name):
                    self.service(filename, name)

    def test_one_shot_services_never_run_as_daemons(self):
        # 迁移与巡检都是一次性任务：重启策略必须 no，且不能发布端口。
        for filename in COMPOSE_FILES:
            for name in ONE_SHOT_SERVICES:
                service = self.service(filename, name)
                with self.subTest(filename=filename, service=name):
                    self.assertEqual(
                        service.get("restart"),
                        "no",
                        describe(filename, name, "restart", "no", service.get("restart")),
                    )
                    if "ports" in service:
                        self.fail(
                            f"{filename} 的 {name} 不得发布端口：实际 ports={service['ports']!r}"
                        )

    def test_contract_number_audit_keys_are_stable_in_both_files(self):
        for filename in COMPOSE_FILES:
            service = self.service(filename, AUDIT_SERVICE)
            with self.subTest(filename=filename, key="command"):
                self.assertEqual(
                    service.get("command"),
                    ["./contract-number-audit"],
                    describe(
                        filename, AUDIT_SERVICE, "command", ["./contract-number-audit"], service.get("command")
                    )
                    + "（必须是参数列表写法，字符串写法在 compose run --rm 下不会按预期切分）",
                )
            dependency = (service.get("depends_on") or {}).get(MYSQL_DEPENDENCY) or {}
            with self.subTest(filename=filename, key="depends_on"):
                self.assertEqual(
                    dependency.get("condition"),
                    "service_healthy",
                    describe(
                        filename,
                        AUDIT_SERVICE,
                        f"depends_on.{MYSQL_DEPENDENCY}.condition",
                        "service_healthy",
                        dependency.get("condition"),
                    )
                    + "（巡检必须在 contract-mysql 健康后执行，否则会把连不上库误判成退出码 1）",
                )
            with self.subTest(filename=filename, key="profiles"):
                if filename == CANONICAL:
                    # 生产真入口由 deploy-service.sh 显式指定服务名，不靠 profile 开关；
                    # 该文件既有测试（test-compose-blocks.py）也断言全文不含 "profiles:"。
                    if "profiles" in service:
                        self.fail(
                            f"{filename} 的 {AUDIT_SERVICE} 不得声明 profiles 键："
                            f"实际 {service['profiles']!r}"
                        )
                else:
                    self.assertEqual(
                        service.get("profiles"),
                        ["release"],
                        describe(filename, AUDIT_SERVICE, "profiles", ["release"], service.get("profiles")),
                    )

    def test_contract_number_audit_matches_contract_migrate(self):
        # 巡检与迁移共用同一个镜像、同一份运行配置文件、同一份 MySQL 依赖：
        # 任一键漂移都会让巡检在另一个配置下运行，结论不可复核。
        for filename in COMPOSE_FILES:
            audit = self.service(filename, AUDIT_SERVICE)
            migrate = self.service(filename, MIGRATE_SERVICE)
            with self.subTest(filename=filename, key="image"):
                self.assertEqual(
                    audit.get("image"),
                    migrate.get("image"),
                    describe(filename, AUDIT_SERVICE, "image", migrate.get("image"), audit.get("image"))
                    + f"（应与同文件的 {MIGRATE_SERVICE} 相同）",
                )
            with self.subTest(filename=filename, key="env_file"):
                audit_references = env_file_references(audit)
                migrate_references = env_file_references(migrate)
                self.assertTrue(
                    migrate_references,
                    f"{filename} 的 {MIGRATE_SERVICE} 未声明任何 env_file：实际 {migrate.get('env_file')!r}",
                )
                self.assertEqual(
                    audit_references,
                    migrate_references,
                    describe(
                        filename,
                        AUDIT_SERVICE,
                        "env_file 有效引用",
                        migrate_references,
                        audit_references,
                    )
                    + f"（应与同文件的 {MIGRATE_SERVICE} 相同）",
                )
            with self.subTest(filename=filename, key="depends_on"):
                audit_dependency = (audit.get("depends_on") or {}).get(MYSQL_DEPENDENCY)
                migrate_dependency = (migrate.get("depends_on") or {}).get(MYSQL_DEPENDENCY)
                self.assertEqual(
                    audit_dependency,
                    migrate_dependency,
                    describe(
                        filename,
                        AUDIT_SERVICE,
                        f"depends_on.{MYSQL_DEPENDENCY}",
                        migrate_dependency,
                        audit_dependency,
                    )
                    + f"（应与同文件的 {MIGRATE_SERVICE} 相同）",
                )


if __name__ == "__main__":
    unittest.main()
