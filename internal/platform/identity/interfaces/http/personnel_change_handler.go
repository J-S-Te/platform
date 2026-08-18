package identityhttp

import (
	"encoding/json"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"net/http"
	"strings"
	"time"
)

type PersonnelChangeHandler struct {
	service *application.PersonnelChangeService
}

func NewPersonnelChangeHandler(s *application.PersonnelChangeService) *PersonnelChangeHandler {
	return &PersonnelChangeHandler{service: s}
}

type personnelCreatePayload struct {
	UserID             string    `json:"user_id"`
	SourceMembershipID string    `json:"source_membership_id"`
	TargetOrgUnitID    string    `json:"target_org_unit_id"`
	TargetPositionID   string    `json:"target_position_id"`
	ChangeType         string    `json:"change_type"`
	Type               string    `json:"type"`
	Reason             string    `json:"reason"`
	ApprovalReference  string    `json:"approval_reference"`
	ApprovalNo         string    `json:"approval_no"`
	EffectiveAt        time.Time `json:"effective_at"`
	EffectiveDate      string    `json:"effective_date"`
}
type personnelTransitionPayload struct {
	ToStatus          string `json:"to_status"`
	ApprovalReference string `json:"approval_reference"`
}

func (h *PersonnelChangeHandler) Create(w http.ResponseWriter, r *http.Request) {
	// 先从认证上下文取得租户和操作人，避免客户端伪造归属或审计身份。
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var x personnelCreatePayload
	// 限制请求体大小，解析失败统一按参数校验错误返回。
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&x) != nil {
		httpresponse.WriteError(w, r, 422, httperror.Validation)
		return
	}
	if x.ChangeType == "" {
		x.ChangeType = x.Type
	}
	if x.ApprovalReference == "" {
		x.ApprovalReference = x.ApprovalNo
	}
	if x.EffectiveAt.IsZero() && x.EffectiveDate != "" {
		x.EffectiveAt, _ = time.Parse("2006-01-02", x.EffectiveDate)
	}
	v, e := h.service.Create(r.Context(), application.PersonnelChangeCreateInput{TenantID: p.Tenant.ID, OperatorID: p.User.ID, UserID: x.UserID, SourceMembershipID: x.SourceMembershipID, TargetOrgUnitID: x.TargetOrgUnitID, TargetPositionID: x.TargetPositionID, ChangeType: x.ChangeType, Reason: x.Reason, ApprovalReference: x.ApprovalReference, EffectiveAt: x.EffectiveAt})
	if e != nil {
		httpresponse.WriteError(w, r, 422, httperror.Validation)
		return
	}
	httpresponse.WriteSuccess(w, r, 201, "操作成功", v)
}

func (h *PersonnelChangeHandler) PreviewDraft(w http.ResponseWriter, r *http.Request) {
	// 草稿预览只读，不创建请求；租户边界仍取自认证主体。
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var x personnelCreatePayload
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&x) != nil {
		httpresponse.WriteError(w, r, 422, httperror.Validation)
		return
	}
	v, err := h.service.PreviewDraft(r.Context(), application.PersonnelChangeCreateInput{
		TenantID: p.Tenant.ID, OperatorID: p.User.ID, UserID: x.UserID,
		SourceMembershipID: x.SourceMembershipID, TargetPositionID: x.TargetPositionID,
	})
	if err != nil {
		httpresponse.WriteError(w, r, 422, httperror.Validation)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "权限影响预览成功", v)
}
func (h *PersonnelChangeHandler) List(w http.ResponseWriter, r *http.Request) {
	// 列表查询始终绑定当前租户，状态筛选仅作为可选条件下传。
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, 401, httperror.Unauthenticated)
		return
	}
	items, e := h.service.List(r.Context(), p.Tenant.ID, r.URL.Query().Get("status"), r.URL.Query().Get("change_type"), r.URL.Query().Get("keyword"))
	if e != nil {
		httpresponse.WriteError(w, r, 500, httperror.Internal)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "操作成功", map[string]any{"items": items, "total": len(items)})
}
func (h *PersonnelChangeHandler) Transition(w http.ResponseWriter, r *http.Request) {
	// 通用转换将状态机和审批/交接/生效时间闸门统一交给应用服务处理。
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, 401, httperror.Unauthenticated)
		return
	}
	var x personnelTransitionPayload
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&x) != nil || strings.TrimSpace(x.ToStatus) == "" {
		httpresponse.WriteError(w, r, 422, httperror.Validation)
		return
	}
	v, e := h.service.Transition(r.Context(), application.PersonnelChangeTransitionInput{TenantID: p.Tenant.ID, OperatorID: p.User.ID, ID: r.PathValue("change_id"), ToStatus: x.ToStatus, ApprovalReference: x.ApprovalReference})
	if e != nil {
		httpresponse.WriteError(w, r, 409, httperror.Conflict)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "操作成功", v)
}
func (h *PersonnelChangeHandler) Preview(w http.ResponseWriter, r *http.Request) {
	// 权限预览按路径中的请求 ID 查询，但租户仍由认证上下文确定。
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, 401, httperror.Unauthenticated)
		return
	}
	v, e := h.service.Preview(r.Context(), p.Tenant.ID, r.PathValue("change_id"))
	if e != nil {
		httpresponse.WriteError(w, r, 404, httperror.NotFound)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "操作成功", v)
}

func (h *PersonnelChangeHandler) Submit(w http.ResponseWriter, r *http.Request) {
	// 固定端点只负责表达意图，实际合法性由统一状态机判定。
	h.transitionFixed(w, r, "PENDING_APPROVAL")
}
func (h *PersonnelChangeHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	h.transitionFixed(w, r, "CANCELLED")
}
func (h *PersonnelChangeHandler) transitionFixed(w http.ResponseWriter, r *http.Request, status string) {
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, 401, httperror.Unauthenticated)
		return
	}
	v, e := h.service.Transition(r.Context(), application.PersonnelChangeTransitionInput{TenantID: p.Tenant.ID, OperatorID: p.User.ID, ID: r.PathValue("change_id"), ToStatus: status})
	if e != nil {
		httpresponse.WriteError(w, r, 409, httperror.Conflict)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "操作成功", v)
}
