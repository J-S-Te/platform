package identityhttp

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
)

var (
	applicationCodePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	roleCodePattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

var employeeCSVHeaders = map[string]string{
	"display_name": "display_name", "姓名": "display_name", "展示姓名": "display_name", "name": "display_name",
	"email": "email", "邮箱": "email", "mail": "email",
	"mobile": "mobile", "手机": "mobile", "手机号": "mobile", "phone": "mobile",
	"status": "status", "状态": "status",
	"organization_name": "organization_name", "组织": "organization_name", "组织名称": "organization_name",
	"position_name": "position_name", "岗位": "position_name", "岗位名称": "position_name",
	"application_roles": "application_roles", "应用角色": "application_roles", "岗位角色": "application_roles", "角色": "application_roles", "roles": "application_roles",
}

type employeeCSVSelection struct {
	SelectedLines []int `json:"selected_lines"`
}

func parseEmployeeCSV(content []byte, selectedLines []int) (employeeBatchCreateRequest, error) {
	reader := csv.NewReader(bytes.NewReader(content))
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = false
	rows := make([][]string, 0, 101)
	lineNumbers := make([]int, 0, 101)
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(record) == 0 {
			return employeeBatchCreateRequest{}, errors.New("invalid CSV")
		}
		line, _ := reader.FieldPos(0)
		if rowBlank(record) {
			continue
		}
		rows = append(rows, record)
		lineNumbers = append(lineNumbers, line)
		if len(rows) > 101 {
			return employeeBatchCreateRequest{}, errors.New("too many CSV rows")
		}
	}
	if len(rows) == 0 {
		return employeeBatchCreateRequest{}, errors.New("empty CSV")
	}

	columns := map[string]int{"display_name": 0, "email": 1, "mobile": 2, "status": 3, "organization_name": 4, "position_name": 5, "application_roles": 6}
	start := 0
	if isEmployeeCSVHeader(rows[0]) {
		columns = map[string]int{"display_name": -1, "email": -1, "mobile": -1, "status": -1, "organization_name": -1, "position_name": -1, "application_roles": -1}
		for index, value := range rows[0] {
			if name, ok := employeeCSVHeaders[normalizeEmployeeCSVHeader(value)]; ok {
				columns[name] = index
			}
		}
		if columns["display_name"] < 0 {
			return employeeBatchCreateRequest{}, errors.New("display name column is required")
		}
		start = 1
	}

	selected := make(map[int]struct{}, len(selectedLines))
	for _, line := range selectedLines {
		if line > 0 {
			selected[line] = struct{}{}
		}
	}
	request := employeeBatchCreateRequest{Items: make([]employeeBatchCreateItemRequest, 0, len(rows)-start)}
	for index := start; index < len(rows); index++ {
		line := lineNumbers[index]
		if len(selected) > 0 {
			if _, ok := selected[line]; !ok {
				continue
			}
		}
		row := rows[index]
		status := normalizeEmployeeCSVStatus(csvColumn(row, columns["status"]))
		email := nullableCSVColumn(row, columns["email"])
		mobile := nullableCSVColumn(row, columns["mobile"])
		item := employeeBatchCreateItemRequest{
			userCreateRequest: userCreateRequest{DisplayName: csvColumn(row, columns["display_name"]), Email: email, Mobile: mobile, Status: &status},
			LineNo:            line, Organization: csvColumn(row, columns["organization_name"]), Position: csvColumn(row, columns["position_name"]),
			ApplicationRoles: parseEmployeeCSVRoles(csvColumn(row, columns["application_roles"])),
		}
		request.Items = append(request.Items, item)
	}
	if len(request.Items) == 0 {
		return employeeBatchCreateRequest{}, errors.New("no selected CSV rows")
	}
	if len(request.Items) > 100 {
		return employeeBatchCreateRequest{}, errors.New("too many selected CSV rows")
	}
	if len(selected) > 0 && len(request.Items) != len(selected) {
		missing := make([]int, 0)
		for line := range selected {
			found := false
			for _, item := range request.Items {
				if item.LineNo == line {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, line)
			}
		}
		sort.Ints(missing)
		return employeeBatchCreateRequest{}, errors.New("selected CSV row does not exist")
	}
	return request, nil
}

func rowBlank(row []string) bool {
	for _, value := range row {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

func normalizeEmployeeCSVHeader(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(value, "\ufeff")))
}

func isEmployeeCSVHeader(row []string) bool {
	for _, value := range row {
		if _, ok := employeeCSVHeaders[normalizeEmployeeCSVHeader(value)]; ok {
			return true
		}
	}
	return false
}

func csvColumn(row []string, index int) string {
	if index < 0 || index >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[index])
}

func nullableCSVColumn(row []string, index int) *string {
	value := csvColumn(row, index)
	if value == "" {
		return nil
	}
	return &value
}

func normalizeEmployeeCSVStatus(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "停用", "DISABLED":
		return "DISABLED"
	default:
		return "ACTIVE"
	}
}

func parseEmployeeCSVRoles(value string) []applicationRoleAssignmentRequest {
	result := make([]applicationRoleAssignmentRequest, 0)
	for _, token := range regexp.MustCompile(`[|；;]`).Split(value, -1) {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		separator := strings.Index(token, "：")
		width := len("：")
		if separator < 0 {
			separator = strings.Index(token, ":")
			width = 1
		}
		if separator <= 0 || separator+width >= len(token) {
			continue
		}
		applicationName, roleName := strings.TrimSpace(token[:separator]), strings.TrimSpace(token[separator+width:])
		if applicationCodePattern.MatchString(applicationName) && roleCodePattern.MatchString(roleName) {
			result = append(result, applicationRoleAssignmentRequest{ApplicationCode: applicationName, RoleCode: roleName})
		} else {
			result = append(result, applicationRoleAssignmentRequest{ApplicationName: applicationName, RoleName: roleName})
		}
	}
	return result
}
