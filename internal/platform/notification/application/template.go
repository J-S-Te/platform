package application

import (
	"html"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

var (
	placeholderPattern = regexp.MustCompile(`\{\{([A-Za-z][A-Za-z0-9_]{0,63})\}\}`)
	codePattern        = regexp.MustCompile(`^[A-Z][A-Z0-9_:-]{0,127}$`)
)

func validateTemplateInput(tenantID, operatorID, code, name string, status domain.TemplateStatus, titleTemplate, bodyTemplate string, variables []domain.VariableDefinition) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(operatorID) == "" || !codePattern.MatchString(normalizeCode(code)) || strings.TrimSpace(name) == "" || utf8.RuneCountInString(strings.TrimSpace(name)) > 128 || !validTemplateStatus(status) {
		return ErrValidation
	}
	return validateRenderingDefinition(titleTemplate, bodyTemplate, variables)
}

func validateTemplateVersionInput(input CreateTemplateVersionInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || strings.TrimSpace(input.TemplateID) == "" {
		return ErrValidation
	}
	return validateRenderingDefinition(input.TitleTemplate, input.BodyTemplate, input.Variables)
}

func validateRenderingDefinition(titleTemplate, bodyTemplate string, variables []domain.VariableDefinition) error {
	if strings.TrimSpace(titleTemplate) == "" || strings.TrimSpace(bodyTemplate) == "" || utf8.RuneCountInString(titleTemplate) > 500 || utf8.RuneCountInString(bodyTemplate) > 16*1024 || len(variables) > 50 {
		return ErrValidation
	}
	definitions := make(map[string]domain.VariableDefinition, len(variables))
	for _, variable := range variables {
		variable.Name = strings.TrimSpace(variable.Name)
		if !placeholderNameValid(variable.Name) || variable.MaxLength < 0 || variable.MaxLength > 4096 {
			return ErrValidation
		}
		if _, exists := definitions[variable.Name]; exists {
			return ErrValidation
		}
		definitions[variable.Name] = variable
	}
	if err := validatePlaceholders(titleTemplate, definitions); err != nil {
		return err
	}
	return validatePlaceholders(bodyTemplate, definitions)
}

func validatePlaceholders(template string, definitions map[string]domain.VariableDefinition) error {
	matches := placeholderPattern.FindAllStringSubmatchIndex(template, -1)
	covered := 0
	for _, match := range matches {
		if _, ok := definitions[template[match[2]:match[3]]]; !ok {
			return ErrValidation
		}
		covered += match[1] - match[0]
	}
	// 移除所有合法占位符后不应残留分隔符；这样能拒绝 {{name}、{{ name }} 或 {{unknown}}
	// 等畸形/未声明表达式，同时不误伤已经验证的变量。
	remaining := placeholderPattern.ReplaceAllString(template, "")
	if strings.Contains(remaining, "{{") || strings.Contains(remaining, "}}") {
		return ErrValidation
	}
	_ = covered
	return nil
}

func renderTemplate(template string, definitions []domain.VariableDefinition, values map[string]string, maximumLength int) (string, error) {
	definitionMap := make(map[string]domain.VariableDefinition, len(definitions))
	for _, definition := range definitions {
		definitionMap[definition.Name] = definition
	}
	if err := validatePlaceholders(template, definitionMap); err != nil {
		return "", err
	}
	for _, definition := range definitions {
		value, provided := values[definition.Name]
		if definition.Required && (!provided || strings.TrimSpace(value) == "") {
			return "", ErrValidation
		}
		if provided && (definition.MaxLength > 0 && utf8.RuneCountInString(value) > definition.MaxLength) {
			return "", ErrValidation
		}
	}
	for name := range values {
		if _, known := definitionMap[name]; !known {
			return "", ErrValidation
		}
	}

	result := placeholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		name := placeholderPattern.FindStringSubmatch(match)[1]
		return html.EscapeString(values[name])
	})
	if utf8.RuneCountInString(result) > maximumLength {
		return "", ErrValidation
	}
	return result, nil
}

func validateCreateInput(input CreateInput) error {
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.OperatorID) == "" || !codePattern.MatchString(normalizeCode(input.TemplateCode)) || !codePattern.MatchString(normalizeCode(input.Category)) || strings.TrimSpace(input.IdempotencyKey) == "" || utf8.RuneCountInString(strings.TrimSpace(input.IdempotencyKey)) > 128 || len(input.Recipients) == 0 || !validTargetURL(input.TargetURL) || !optionalCode(input.ReferenceType, 64) || utf8.RuneCountInString(strings.TrimSpace(input.ReferenceID)) > 128 {
		return ErrValidation
	}
	for _, recipient := range input.Recipients {
		if !validRecipientType(recipient.Type) || strings.TrimSpace(recipient.ID) == "" {
			return ErrValidation
		}
	}
	return nil
}

func normalizeRecipients(recipients []domain.RecipientTarget) []domain.RecipientTarget {
	seen := make(map[string]struct{}, len(recipients))
	result := make([]domain.RecipientTarget, 0, len(recipients))
	for _, recipient := range recipients {
		recipient.ID = strings.TrimSpace(recipient.ID)
		key := string(recipient.Type) + "\x00" + recipient.ID
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, recipient)
	}
	return result
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneVariables(variables []domain.VariableDefinition) []domain.VariableDefinition {
	cloned := make([]domain.VariableDefinition, len(variables))
	copy(cloned, variables)
	return cloned
}

func validTemplateStatus(status domain.TemplateStatus) bool {
	return status == domain.TemplateStatusActive || status == domain.TemplateStatusDisabled
}

func validRecipientType(recipientType domain.RecipientType) bool {
	return recipientType == domain.RecipientTypeUser || recipientType == domain.RecipientTypeRole || recipientType == domain.RecipientTypeOrganization
}

func placeholderNameValid(name string) bool {
	return regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`).MatchString(name)
}

func normalizeCode(value string) string { return strings.ToUpper(strings.TrimSpace(value)) }

func optionalCode(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	return value == "" || (utf8.RuneCountInString(value) <= maximum && codePattern.MatchString(normalizeCode(value)))
}

func validTargetURL(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || (strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") && !strings.ContainsAny(value, "\r\n"))
}
