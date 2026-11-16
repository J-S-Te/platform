package licensehttp

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	corecatalog "github.com/J-S-Te/license-core/catalog"
)

// RuntimeApplication 是前端业务系统下拉所需的已接入系统投影。
type RuntimeApplication struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
}

// RuntimeApplicationLister 由应用注册/部署状态读模型实现；只返回当前租户已接入系统。
type RuntimeApplicationLister interface {
	ListRuntimeApplications(context.Context, string, string) ([]RuntimeApplication, error)
}

type runtimeApplicationsResponse struct {
	Items []RuntimeApplication `json:"items"`
}

// ConfigureRuntimeApplications 挂接已接入系统探测来源；nil 表示无法列出，读取接口返回 503。
func (h *RuntimeHandler) ConfigureRuntimeApplications(lister RuntimeApplicationLister) {
	h.applications = lister
}

// Applications 用途：列出当前租户/环境下已部署且已接入的商业授权业务系统。
// 参数：w 为响应，r 为带 platform:license:read 的读取请求。
// 返回值：无；探测失败不伪造空清单，只返回固定错误码。
func (h *RuntimeHandler) Applications(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r, false)
	if !ok {
		return
	}
	if h.applications == nil || h.environment == "" {
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable,
			httperror.New("LICENSE_RUNTIME_APPLICATIONS_UNSUPPORTED", "尚未配置已接入系统探测来源", nil))
		return
	}
	rows, err := h.applications.ListRuntimeApplications(r.Context(), actor.TenantID, h.environment)
	if err != nil {
		// 存储错误可能包含内部细节；对外只暴露固定错误码，避免泄露 SQL/部署信息。
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable,
			httperror.New("LICENSE_RUNTIME_APPLICATIONS_UNAVAILABLE", "已接入系统探测暂时不可用，请稍后重试", nil))
		return
	}
	items := make([]RuntimeApplication, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		code := strings.TrimSpace(row.Code)
		name := strings.TrimSpace(row.Name)
		environment := strings.TrimSpace(row.Environment)
		if code == "" || name == "" || environment == "" || environment != h.environment {
			continue
		}
		if _, exists := seen[code]; exists {
			continue
		}
		// 只有内置商业授权目录支持的应用才能进入运行时授权页；自定义应用没有
		// 可核验的组件目录，不能在此处被当成可授权系统。
		if _, err := corecatalog.Components(code); err != nil {
			continue
		}
		seen[code] = struct{}{}
		items = append(items, RuntimeApplication{Code: code, Name: name, Environment: environment})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Code < items[j].Code })
	w.Header().Set("Cache-Control", "no-store")
	httpresponse.WriteSuccess(w, r, http.StatusOK, "当前租户已接入的商业授权业务系统", runtimeApplicationsResponse{Items: items})
}
