package identityhttp

import (
	"encoding/json"
	"errors"
	authzapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"net/http"
	"strings"
	"time"
)

// PersonnelChangeHandler 提供人事变更（调动/交接/审批状态机）的 HTTP 入口；
// 租户与操作人一律取自认证上下文。
type PersonnelChangeHandler struct {
	service *application.PersonnelChangeService
}

// NewPersonnelChangeHandler 构造人事变更处理器。
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
	// Immediate 表示对已排期请求跳过生效时间闸门立即执行；合法性由应用服务校验。
	Immediate bool `json:"immediate"`
	// ExceptionReason 仅用于超管例外审批的留痕说明；是否成立由服务端主体角色决定，
	// 浏览器无法通过该字段获得豁免。
	ExceptionReason string `json:"exception_reason"`
}
type personnelHandoverCompletePayload struct {
	TargetUserID string `json:"target_user_id"`
}

// Create 创建人事变更请求；直接排期能力仅授予平台超级管理员，其余操作者
// 创建后进入常规审批流。
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
	v, e := h.service.Create(r.Context(), application.PersonnelChangeCreateInput{TenantID: p.Tenant.ID, OperatorID: p.User.ID, UserID: x.UserID, SourceMembershipID: x.SourceMembershipID, TargetOrgUnitID: x.TargetOrgUnitID, TargetPositionID: x.TargetPositionID, ChangeType: x.ChangeType, Reason: x.Reason, ApprovalReference: x.ApprovalReference, EffectiveAt: x.EffectiveAt, DirectScheduleAuthorized: principalHasRole(p, "platform-super-admin")})
	if e != nil {
		writePersonnelChangeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 201, "操作成功", v)
}

func principalHasRole(principal authctx.Principal, expected string) bool {
	for _, role := range principal.Roles {
		if strings.EqualFold(strings.TrimSpace(role.Code), expected) {
			return true
		}
	}
	return false
}

// PreviewDraft 对尚未创建的变更草稿做只读权限影响预览，不落任何请求记录。
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

// List 返回当前租户的人事变更请求列表，支持状态、类型与关键字可选过滤。
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

// Transition 执行通用状态机转换；超管例外审批与立即生效的合法性均由应用服务
// 依据服务端主体角色校验，载荷中的例外原因仅用于留痕。
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
	// 超管例外只能由服务端依据认证主体的角色推导；浏览器载荷不得决定豁免。
	v, e := h.service.Transition(r.Context(), application.PersonnelChangeTransitionInput{
		TenantID: p.Tenant.ID, OperatorID: p.User.ID, ID: r.PathValue("change_id"),
		ToStatus: x.ToStatus, ApprovalReference: x.ApprovalReference, Immediate: x.Immediate,
		ApproverIsPrivileged: principalHasRole(p, application.BootstrapSuperAdminRoleCode),
		ExceptionReason:      x.ExceptionReason,
	})
	if e != nil {
		writePersonnelChangeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "操作成功", v)
}

// Preview 返回指定变更请求的权限影响预览；请求不存在时返回 404。
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

// Submit 提交变更请求进入待审批状态；合法性由统一状态机判定。
func (h *PersonnelChangeHandler) Submit(w http.ResponseWriter, r *http.Request) {
	// 固定端点只负责表达意图，实际合法性由统一状态机判定。
	h.transitionFixed(w, r, "PENDING_APPROVAL")
}

// Cancel 取消变更请求；仅允许状态机许可的状态下执行。
func (h *PersonnelChangeHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	h.transitionFixed(w, r, "CANCELLED")
}

// ListHandoverItems 返回指定变更请求的交接项清单。
func (h *PersonnelChangeHandler) ListHandoverItems(w http.ResponseWriter, r *http.Request) {
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	items, err := h.service.ListHandoverItems(r.Context(), p.Tenant.ID, r.PathValue("change_id"))
	if err != nil {
		writePersonnelChangeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "交接项加载成功", map[string]any{"items": items, "total": len(items), "cross_system_enabled": h.service.HandoverOrchestrationEnabled()})
}

// CompleteHandoverItem 把单个交接项的责任转移给目标接收人。
func (h *PersonnelChangeHandler) CompleteHandoverItem(w http.ResponseWriter, r *http.Request) {
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var payload personnelHandoverCompletePayload
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&payload) != nil || strings.TrimSpace(payload.TargetUserID) == "" {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	item, err := h.service.CompleteHandoverItem(r.Context(), p.Tenant.ID, r.PathValue("change_id"), r.PathValue("item_id"), payload.TargetUserID, p.User.ID)
	if err != nil {
		writePersonnelChangeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "责任交接状态已更新", item)
}

// RetryHandover 只允许已认证且有审批权限的入口恢复批次发现。
func (h *PersonnelChangeHandler) RetryHandover(w http.ResponseWriter, r *http.Request) {
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	batch, err := h.service.RetryHandover(r.Context(), p.Tenant.ID, r.PathValue("change_id"))
	if err != nil {
		writePersonnelChangeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "交接发现状态已更新", batch)
}

// transitionFixed 服务固定状态转移（无请求载荷）。该端点不携带例外原因，因此超管例外
// 审批必须走通用 Transition 端点：服务端特权主体在这里没有原因可留痕时会被校验拒绝
// （fail-closed），普通审批人的自审则一律被 ErrSelfApprovalForbidden 拒绝。
func (h *PersonnelChangeHandler) transitionFixed(w http.ResponseWriter, r *http.Request, status string) {
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, 401, httperror.Unauthenticated)
		return
	}
	v, e := h.service.Transition(r.Context(), application.PersonnelChangeTransitionInput{TenantID: p.Tenant.ID, OperatorID: p.User.ID, ID: r.PathValue("change_id"), ToStatus: status})
	if e != nil {
		writePersonnelChangeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, 200, "操作成功", v)
}

func writePersonnelChangeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, authzapplication.ErrForbidden), errors.Is(err, application.ErrSelfApprovalForbidden):
		httpresponse.WriteError(w, r, http.StatusForbidden, httperror.Forbidden)
	case errors.Is(err, application.ErrValidation):
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrHandoverFreezeReleased):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.New("IAM_HANDOVER_FREEZE_RELEASED", "该离职单的交接冻结已提前解除，不能直接执行。请联系管理员受控恢复交接冻结并重新核验当前业务责任；不要重复点击立即执行。", nil))
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, bootstrapadmin.ErrProtected):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.BootstrapAdminProtected)
	case errors.Is(err, bootstrapadmin.ErrConfiguration):
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.BootstrapAdminConfiguration)
	default:
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}
