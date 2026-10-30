package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type foundationAccessImageRunner struct {
	metabaseImage string
	refs          []string
	missing       bool
	mysqlRefs     []string
}

// Run 模拟本机Docker引擎可用，不执行真实容器变更。
// 参数：其余参数为原生命令边界，在本用例不产生外部副作用。
// 返回值：无错误，镜像内容由RunOutput单独核验。
func (r *foundationAccessImageRunner) Run(context.Context, string, []string, string, ...string) error {
	return nil
}

// RunOutput 提供受审数析拓扑与逐个镜像的真实协议形状。
// 参数：args为Compose或Docker镜像只读查询参数。
// 返回值：拓扑或RepoDigests；缺失镜像显式报错。
func (r *foundationAccessImageRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	if containsString(args, "--services") {
		return []byte("data-analysis-api\ndata-analysis-metabase\ndata-analysis-metabase-init\ndata-analysis-migrate\n"), nil
	}
	if len(args) > 1 && args[0] == "image" {
		image := args[len(args)-1]
		if image == r.metabaseImage {
			if r.missing {
				return nil, errors.New("image absent")
			}
			return json.Marshal(r.refs)
		}
		if image == "mysql:8.4" {
			if r.mysqlRefs != nil {
				return json.Marshal(r.mysqlRefs)
			}
			return json.Marshal([]string{"mysql@sha256:" + strings.Repeat("c", 64)})
		}
		return json.Marshal([]string{"example/analysis@sha256:" + strings.Repeat("a", 64)})
	}
	services := map[string]any{}
	for _, name := range []string{"data-analysis-api", "data-analysis-migrate"} {
		services[name] = map[string]string{"image": "example/analysis@sha256:" + strings.Repeat("a", 64)}
	}
	services["data-analysis-metabase"] = map[string]string{"image": r.metabaseImage}
	services["data-analysis-metabase-init"] = map[string]string{"image": "mysql:8.4"}
	return json.Marshal(map[string]any{"services": services})
}

// foundationImageFixture 构造与受审数析清单一致的独立镜像检查器。
// 参数：t为测试上下文，runner提供Docker协议边界。
// 返回值：生产Agent读取器；不连接真实Docker或数据库。
func foundationImageFixture(t *testing.T, runner *foundationAccessImageRunner) *ProductionComposeSubsystemProvisioner {
	t.Helper()
	target, _ := runtimeLicenseFixture(t)
	target.config.Profile.Manifest.Application.Code = "data_analysis"
	target.config.Profile.Manifest.Compose.RuntimeServices = []string{"data-analysis-api", "data-analysis-metabase"}
	target.config.Profile.Manifest.Compose.InitializationServices = []string{"data-analysis-metabase-init"}
	target.config.Profile.Manifest.Compose.MigrateService = "data-analysis-migrate"
	target.config.DockerBinary = "docker"
	target.runner = runner
	return &ProductionComposeSubsystemProvisioner{enabled: true,
		targets: map[string]*productionComposeTarget{productionSubsystemTargetKey("data_analysis", "prod"): target}}
}

// TestReviewedFoundationImagesResolveImmutableIdentities 覆盖固定基础镜像的真实digest核验。
// 参数：t为本地只读协议测试上下文。
// 返回值：无；来源、版本、摘要或可用性不满足时不能报告就绪。
func TestReviewedFoundationImagesResolveImmutableIdentities(t *testing.T) {
	valid := "metabase/metabase@sha256:" + strings.Repeat("b", 64)
	for _, scenario := range []string{"present", "alias", "mysql_alias", "missing", "foreign", "nested_alias", "nested_docker_alias", "ambiguous", "mutable"} {
		t.Run(scenario, func(t *testing.T) {
			runner := &foundationAccessImageRunner{metabaseImage: "metabase/metabase:v0.53.7", refs: []string{valid}}
			switch scenario {
			case "alias":
				runner.refs = []string{"docker.io/" + valid, valid}
			case "mysql_alias":
				runner.mysqlRefs = []string{"docker.io/library/mysql@sha256:" + strings.Repeat("c", 64)}
			case "missing":
				runner.missing = true
			case "foreign":
				runner.refs = []string{"other/metabase@sha256:" + strings.Repeat("b", 64)}
			case "nested_alias":
				runner.refs = []string{"library/" + valid}
			case "nested_docker_alias":
				runner.refs = []string{"docker.io/library/" + valid}
			case "ambiguous":
				runner.refs = append(runner.refs, "metabase/metabase@sha256:"+strings.Repeat("d", 64))
			case "mutable":
				runner.metabaseImage = "metabase/metabase:latest"
			}
			p := foundationImageFixture(t, runner)
			images, err := p.SubsystemImageReadiness(context.Background(), "data_analysis", "prod")
			if scenario == "foreign" || scenario == "nested_alias" || scenario == "nested_docker_alias" || scenario == "ambiguous" || scenario == "mutable" {
				if err == nil {
					t.Fatal("unreviewed or unbound foundation identity accepted")
				}
				return
			}
			if err != nil || len(images) != 4 {
				t.Fatalf("reviewed foundation inventory: %v %v", images, err)
			}
			for _, item := range images {
				if item.ServiceID == "data-analysis-metabase" && scenario == "missing" {
					if item.Present {
						t.Fatal("missing foundation reported as present")
					}
					continue
				}
				if !item.Present || !isImmutableProductionImageReference(item.ImageDigest) {
					t.Fatalf("readiness lacks verified immutable identity: %#v", item)
				}
			}
		})
	}
}

// TestFoundationDigestChangesInvalidateCandidateIdentity 确保同标签不同内容能使候选身份改变。
// 参数：t为镜像摘要变更的独立测试上下文。
// 返回值：无；新摘要不得复用旧候选内容。
func TestFoundationDigestChangesInvalidateCandidateIdentity(t *testing.T) {
	runner := &foundationAccessImageRunner{metabaseImage: "metabase/metabase:v0.53.7",
		refs: []string{"metabase/metabase@sha256:" + strings.Repeat("b", 64)}}
	p := foundationImageFixture(t, runner)
	first, err := p.SubsystemImageReadiness(context.Background(), "data_analysis", "prod")
	if err != nil {
		t.Fatal(err)
	}
	runner.refs = []string{"metabase/metabase@sha256:" + strings.Repeat("d", 64)}
	second, err := p.SubsystemImageReadiness(context.Background(), "data_analysis", "prod")
	if err != nil {
		t.Fatal(err)
	}
	var before, after application.SubsystemImageReadiness
	for _, image := range first {
		if image.ServiceID == "data-analysis-metabase" {
			before = image
		}
	}
	for _, image := range second {
		if image.ServiceID == "data-analysis-metabase" {
			after = image
		}
	}
	if before.ImageDigest == after.ImageDigest {
		t.Fatal("foundation content changed without candidate identity change")
	}
}
