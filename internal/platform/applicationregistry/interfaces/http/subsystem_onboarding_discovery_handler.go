// subsystem_onboarding_discovery_handler.go：子系统候选发现（discovery）——只读暴露租户内未登记容器的自愿申报元数据。
package http

import (
	stdhttp "net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// DiscoverSubsystemCandidates 仅暴露租户内尚未登记容器的自愿申报 Docker 标签元数据。
// 已登记判定按应用身份匹配，既不按候选自报的环境，也不按当前用户的门户可见性。
// 本端点绝不执行部署命令，因此可以独立于接入权限、仅授予应用读权限。
func (handler *SubsystemOnboardingHandler) DiscoverSubsystemCandidates(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	discoverer, ok := handler.provisioner.(subsystemCandidateDiscoverer)
	if !ok {
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	candidates, err := discoverer.DiscoverSubsystemCandidates(request.Context())
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	registered, err := handler.service.ListRegisteredApplicationCodes(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	existing := make(map[string]struct{}, len(registered))
	for _, code := range registered {
		existing[strings.ToLower(strings.TrimSpace(code))] = struct{}{}
	}
	result := make([]application.SubsystemDiscoveryCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate.ApplicationCode))
		if _, found := existing[key]; !found {
			result = append(result, candidate)
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "发现未登记子系统成功", result)
}
