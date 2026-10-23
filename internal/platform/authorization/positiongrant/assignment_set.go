package positiongrant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"
)

// AssignmentSetView 把可编辑快照绑定到租户/岗位维度的 revision。已停用或隐藏的行
// 同样参与计算，保证空编辑器场景下的并发安全。
type AssignmentSetView struct {
	Items    []AssignmentView `json:"items"`
	Total    int              `json:"total"`
	Revision string           `json:"revision"`
}

func assignmentSetRevision(tenantID, positionID string, rows []assignmentModel) string {
	type item struct {
		ID, TemplateID, Status, From, Until string
		Version                             uint64
	}
	values := make([]item, 0, len(rows))
	for _, r := range rows {
		values = append(values, item{r.ID, r.TemplateID, r.Status, assignmentTime(r.ValidFrom), assignmentTime(r.ValidUntil), r.Version})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	raw, _ := json.Marshal(struct {
		TenantID, PositionID string
		Items                []item
	}{tenantID, positionID, values})
	sum := sha256.Sum256(raw)
	return "v1:" + hex.EncodeToString(sum[:])
}
func assignmentTime(v *time.Time) string {
	if v == nil {
		return ""
	}
	return v.UTC().Format(time.RFC3339Nano)
}
func (s *Service) readAssignmentSet(ctx context.Context, tx *gorm.DB, tenantID, positionID string) (AssignmentSetView, error) {
	var rows []assignmentModel
	if err := tx.WithContext(ctx).Where("tenant_id=? AND position_id=?", tenantID, positionID).Order("created_at DESC,id DESC").Find(&rows).Error; err != nil {
		return AssignmentSetView{}, fmt.Errorf("list position template assignments: %w", err)
	}
	reader := *s
	reader.db = tx
	items := make([]AssignmentView, 0, len(rows))
	for _, r := range rows {
		template, err := reader.get(ctx, tenantID, r.TemplateID)
		if err != nil {
			return AssignmentSetView{}, err
		}
		if template.Status != activeStatus {
			continue
		}
		items = append(items, AssignmentView{AssignmentID: r.ID, PositionID: r.PositionID, Template: template, Status: r.Status, ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil, Version: r.Version})
	}
	return AssignmentSetView{Items: items, Total: len(items), Revision: assignmentSetRevision(tenantID, positionID, rows)}, nil
}
