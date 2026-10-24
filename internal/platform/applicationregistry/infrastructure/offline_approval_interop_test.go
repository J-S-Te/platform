package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

// 此夹具在临时模块中调用真实工厂准备实现，
// 再将生成的 JSON 交给生产 Agent 读取器。此处的镜像事实和
// 覆盖范围明确属于合成协议夹具，不得作为服务器审核
// 输入。测试不执行 Docker 命令、修改仓库注册信息或签发许可证。
const offlineApprovalFactoryBridge = `package main
import (
 "context"
 "os"
 "path/filepath"
 "github.com/J-S-Te/commercial-license-tool/internal/runtimeapproval"
 "github.com/J-S-Te/license-core/coverage"
)
type facts struct { Images map[string]runtimeapproval.Image }
func (f facts) Inspect(_ context.Context, ref string) (runtimeapproval.Image,error) {
 image,ok:=f.Images[ref]; if !ok { return image,runtimeapproval.ErrApproval }; return image,nil
}
func (f facts) InspectPlatform(ctx context.Context, ref string) (string,error) {
 _,err:=f.Inspect(ctx,ref); return "amd64/linux",err
}
func main() {
 var in struct { Scenario string; Inventory runtimeapproval.Inventory; Mapping runtimeapproval.Mapping; Applications []string; Images map[string]runtimeapproval.Image }
 check:=func(err error){ if err!=nil { panic(err) } }
 raw,err:=os.ReadFile(os.Args[2]);check(err);check(runtimeapproval.DecodeJSON(raw,&in))
 profiles:=[]runtimeapproval.Profile{}
 registry:=coverage.Registry{Schema:1,Scope:"synthetic protocol interop, not release acceptance",SourceDigests:map[string]string{}}
 for _,name:=range in.Mapping.Profiles {
  raw,err:=os.ReadFile(filepath.Join(os.Args[1],"subsystems.d",name));check(err)
  p,err:=runtimeapproval.ParseProfile(raw);check(err);profiles=append(profiles,p)
  source:=p.Application+"/synthetic-interop.go";registry.SourceDigests[source]="synthetic-source"
  registry.Entries=append(registry.Entries,coverage.Entry{ID:p.Application,Application:p.Application,Source:source,Kind:"http",Registration:"synthetic-interop",Operation:"READ_HISTORY",Reason:"protocol fixture only"})
 }
 var bundle runtimeapproval.Bundle
 inspector:=facts{in.Images}
 switch in.Scenario {
 case "fresh": bundle,err=runtimeapproval.PrepareFresh(context.Background(),in.Inventory,in.Mapping,profiles,registry,inspector,in.Applications)
 case "expand": bundle,err=runtimeapproval.PrepareExpansion(context.Background(),in.Inventory,in.Mapping,profiles,registry,inspector,in.Applications)
 case "legacy":
  var before runtimeapproval.Evidence
  bundle,before,err=runtimeapproval.PrepareLegacyCRM(context.Background(),in.Inventory,profiles,registry,inspector,in.Mapping.Images["customer-api"].Reference)
  bundle.Migration=&before
 case "full": bundle,err=runtimeapproval.Prepare(context.Background(),in.Mapping,profiles,registry,inspector)
 default: panic("unknown synthetic scenario")
 }
 check(err);bundle.CandidateOnly=true;check(runtimeapproval.WriteBundle(os.Args[3],bundle,"synthetic-interop"))
}
`

func offlineInteropProfiles(t *testing.T) (string, string, []productionSubsystemProfile) {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	platformRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../.."))
	workspace, productionRoot := filepath.Dir(platformRoot), filepath.Join(platformRoot, "deploy/production")
	profiles, _, err := loadProductionSubsystemProfiles(productionRoot, "")
	if err != nil || len(profiles) != 6 {
		t.Fatalf("actual six production profiles required: %v", err)
	}
	return workspace, productionRoot, profiles
}

func TestOfflineFactoryApprovalsInteroperateWithProductionAgent(t *testing.T) {
	if os.Getenv("UIP_FACTORY_APPROVAL_INTEROP") != "1" {
		t.Skip("requires UIP_FACTORY_APPROVAL_INTEROP=1 and the adjacent factory/license-core workspace; native Agent JSON boundary tests run independently")
	}
	workspace, productionRoot, profiles := offlineInteropProfiles(t)
	for _, module := range []string{"commercial-license-tool", "license-core"} {
		if _, err := os.Stat(filepath.Join(workspace, module, "go.mod")); err != nil {
			t.Fatalf("BLOCKED: enabled factory interop requires adjacent %s module: %v", module, err)
		}
	}
	temp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(temp, "factory-bridge")
	if err = os.Mkdir(bridge, 0700); err != nil {
		t.Fatal(err)
	}
	mod := fmt.Sprintf("module github.com/J-S-Te/commercial-license-tool/offlineinterop\n\ngo 1.25.4\n\nrequire github.com/J-S-Te/commercial-license-tool v0.0.0\nrequire github.com/J-S-Te/license-core v0.0.0\nreplace github.com/J-S-Te/commercial-license-tool => %q\nreplace github.com/J-S-Te/license-core => %q\n", filepath.ToSlash(filepath.Join(workspace, "commercial-license-tool")), filepath.ToSlash(filepath.Join(workspace, "license-core")))
	for name, raw := range map[string]string{"go.mod": mod, "main.go": offlineApprovalFactoryBridge} {
		if err = os.WriteFile(filepath.Join(bridge, name), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, scenario := range []string{"fresh", "legacy", "expand", "full"} {
		t.Run(scenario, func(t *testing.T) {
			input := offlineInteropInput(profiles, scenario)
			inputPath, output := filepath.Join(temp, scenario+".json"), filepath.Join(temp, scenario+"-candidate")
			raw, err := json.Marshal(input)
			if err != nil || os.WriteFile(inputPath, raw, 0600) != nil {
				t.Fatal("write synthetic factory fixture", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, "go", "run", "-mod=mod", ".", productionRoot, inputPath, output)
			command.Dir = bridge
			command.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
			if result, err := command.CombinedOutput(); err != nil {
				t.Fatalf("real factory preparation failed: %v\n%s", err, result)
			}
			actual, err := loadInstallationLicenseEvidenceApproval(filepath.Join(output, "license-evidence.json"), "synthetic-interop", profiles)
			if err != nil {
				t.Fatal("Agent rejected factory installation evidence", err)
			}
			for _, profile := range profiles {
				path := filepath.Join(output, "runtime-license-"+profile.Manifest.Application.Code+"-prod.json")
				if _, err := os.Stat(path); os.IsNotExist(err) {
					continue
				}
				release, err := loadRuntimeLicenseReleaseDocument(path, profile)
				if err != nil {
					t.Fatal("Agent rejected full fixed-profile release", profile.Manifest.Application.Code, err)
				}
				if profile.Manifest.Application.Code == "customer_and_opportunity" && (len(release.Components) != 9 || !slices.ContainsFunc(release.Components, func(item application.RuntimeLicenseApproval) bool {
					return item.ServiceID == "customer-contract-transfer-worker"
				})) {
					t.Fatal("current CRM release must include the ninth independent consumer")
				}
				if scenario == "expand" && profile.Manifest.Application.Code == "customer_portal" && len(release.Components) != 3 {
					t.Fatal("absent conditional process was removed from full Portal declaration")
				}
			}
			if scenario != "full" {
				before, err := loadMigrationInstallationEvidenceApproval(filepath.Join(output, "license-migration-evidence.json"), "synthetic-interop", profiles)
				if err != nil {
					t.Fatal("Agent rejected actual pre-installation boundary", err)
				}
				if scenario == "fresh" && len(before.Services) != 0 {
					t.Fatal("fresh acquired migration members")
				}
				if scenario == "legacy" && (len(before.Services) != 8 || slices.ContainsFunc(before.Services, func(item licenseevidence.Service) bool { return item.Name == "customer-contract-transfer-worker" })) {
					t.Fatal("legacy eight-member installation acquired fictitious ninth member")
				}
				if scenario == "expand" {
					optional := "portal-invite-compensation-worker"
					for _, services := range [][]licenseevidence.Service{actual.Services, before.Services} {
						if slices.ContainsFunc(services, func(item licenseevidence.Service) bool { return item.Name == optional }) {
							t.Fatal("absent conditional consumer was fabricated as running or legacy")
						}
					}
					if len(before.Services) != 11 {
						t.Fatal("expansion changed actual CRM/Portal boundary")
					}
				}
			}
		})
	}
}

func offlineInteropInput(profiles []productionSubsystemProfile, scenario string) map[string]any {
	const project = "synthetic-interop"
	oldID, targetID, metabaseID := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("e", 64)
	now := time.Now().UTC().Add(-time.Minute)
	containers := []map[string]any{}
	appendContainer := func(name string, business bool) {
		version, protocol := "", ""
		if business && scenario != "legacy" {
			version, protocol = "synthetic-old-v1", "1"
		}
		containers = append(containers, map[string]any{"container_id": fmt.Sprintf("%064x", len(containers)+1), "service": name, "image_digest": oldID, "version": version, "protocol": protocol, "running": true, "one_off": false, "started_at": now.Add(-time.Hour).Format(time.RFC3339Nano), "architecture": "amd64", "os": "linux"})
	}
	appendContainer("platform-api", false)
	filenames, images := []string{}, map[string]any{}
	for _, profile := range profiles {
		app := profile.Manifest.Application.Code
		filename := app + "-prod.yaml"
		if app == "data_analysis" {
			filename = "data-analysis-prod.yaml"
		}
		filenames = append(filenames, filename)
		selected := scenario == "full" || app == "customer_and_opportunity" || (scenario != "legacy" && app != "customer_portal" && app != "settlement") || (scenario == "expand" && app == "customer_portal")
		if !selected {
			continue
		}
		for _, name := range profile.Manifest.Compose.RuntimeServices {
			ref := targetID
			if name == "data-analysis-metabase" {
				ref = metabaseID
			}
			existing := app == "customer_and_opportunity" || (app == "customer_portal" && name != "portal-invite-compensation-worker")
			if scenario == "expand" && existing {
				ref = oldID
				appendContainer(name, true)
			}
			if scenario == "legacy" && name != "customer-contract-transfer-worker" {
				appendContainer(name, true)
			}
			images[name] = map[string]any{"reference": ref, "replicas": 1}
		}
	}
	applications := []string{"customer_and_opportunity", "contract_management", "project_management", "data_analysis"}
	if scenario == "expand" {
		applications = applications[1:]
	}
	return map[string]any{"Scenario": scenario, "Applications": applications,
		"Inventory": map[string]any{"version": 1, "project": project, "deploy_root": "/nonexistent/synthetic-interop", "collected_at": now.Format(time.RFC3339Nano), "compose_digest": oldID, "release_digest": oldID, "containers": containers},
		"Mapping":   map[string]any{"version": 1, "project": project, "profiles": filenames, "images": images, "excluded_services": []string{"platform-api"}},
		"Images":    map[string]any{oldID: map[string]any{"id": oldID, "version": "synthetic-old-v1", "protocol": "1"}, targetID: map[string]any{"id": targetID, "version": "synthetic-target-v2", "protocol": "1"}, metabaseID: map[string]any{"id": metabaseID}}}
}
