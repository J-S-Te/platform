package applicationaccess

import "testing"

func TestNormalizeCatalogCanonicalizesAndHashesDeterministically(t *testing.T) {
	first, checksum1, err := normalizeCatalog(CatalogInput{
		CatalogVersion: " 2026.07.29 ",
		Permissions: []PermissionInput{
			{Code: "customer.read", Name: "查看客户", Action: "read", ResourceCode: "customer"},
			{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"},
		},
		Roles: []CatalogRoleInput{{Code: "sales", Name: "销售", Permissions: []string{"customer.read", "contract.read", "customer.read"}}},
	})
	if err != nil {
		t.Fatalf("normalizeCatalog returned error: %v", err)
	}
	second, checksum2, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "2026.07.29",
		Permissions: []PermissionInput{
			{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract", RiskLevel: "LOW"},
			{Code: "customer.read", Name: "查看客户", Action: "read", ResourceCode: "customer", RiskLevel: "LOW"},
		},
		Roles: []CatalogRoleInput{{Code: "sales", Name: "销售", Permissions: []string{"contract.read", "customer.read"}}},
	})
	if err != nil {
		t.Fatalf("normalizeCatalog returned error: %v", err)
	}
	if checksum1 != checksum2 || len(first.Permissions) != 2 || len(second.Roles[0].Permissions) != 2 {
		t.Fatalf("catalog normalization is not deterministic: %#v %s %#v %s", first, checksum1, second, checksum2)
	}
	if checksum1 == "" {
		t.Fatalf("checksum should not be empty")
	}
}

func TestNormalizeCatalogRejectsUnknownRolePermission(t *testing.T) {
	_, _, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "1",
		Permissions:    []PermissionInput{{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"}},
		Roles:          []CatalogRoleInput{{Code: "sales", Name: "销售", Permissions: []string{"contract.write"}}},
	})
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestNormalizeCatalogRejectsDuplicateResourceAction(t *testing.T) {
	_, _, err := normalizeCatalog(CatalogInput{
		CatalogVersion: "1",
		Permissions: []PermissionInput{
			{Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract"},
			{Code: "contract.view", Name: "浏览合同", Action: "READ", ResourceCode: "contract"},
		},
	})
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}
