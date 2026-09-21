package filetaskhttp

import (
	"crypto/sha256"
	"testing"
)

func TestUploadPoliciesUseStableLocalNamespaces(t *testing.T) {
	expected := map[string]string{
		"platform.iam.user-import":          "platform/iam-user-import",
		"crm.customer.import":               "crm/customer-import",
		"crm.opportunity.attachment":        "crm/opportunity-attachment",
		"portal.filing.material":            "portal/filing-material",
		"contract.external.source":          "contract/external-source",
		"contract.template":                 "contract/template",
		"contract.stamped-pdf":              "contract/stamped-pdf",
		"project.field.evidence":            "project/field-evidence",
		"project.deviation.evidence":        "project/deviation-evidence",
		"project.report":                    "project/report",
		"project.capability.import":         "project/capability-import",
		"project.detection-category.import": "project/detection-category-import",
		"settlement.invoice.document":       "settlement/invoice-document",
	}
	for key, path := range expected {
		item, ok := uploadPolicies[key]
		if !ok {
			t.Fatalf("missing upload policy %s", key)
		}
		if got := item.Namespace + "/" + item.Purpose; got != path {
			t.Fatalf("policy %s path=%s want=%s", key, got, path)
		}
		if item.MaxBytes == 0 || len(item.Media) == 0 {
			t.Fatalf("policy %s is incomplete", key)
		}
	}
}

func TestPurposeCannotCrossApplicationBoundary(t *testing.T) {
	for _, item := range []struct {
		code, namespace string
		want            bool
	}{
		{"customer_and_opportunity", "crm", true}, {"customer_portal", "portal", true},
		{"contract_management", "contract", true}, {"project_management", "project", true},
		{"settlement", "settlement", true}, {"contract_management", "crm", false},
		{"data_analysis", "project", false}, {"", "platform", false},
	} {
		if got := validPurposeApplication(item.code, item.namespace); got != item.want {
			t.Fatalf("validPurposeApplication(%q,%q)=%v want=%v", item.code, item.namespace, got, item.want)
		}
	}
}

func TestTicketsAreOpaqueAndStoredOnlyAsHashes(t *testing.T) {
	raw, hash, err := randomTicket()
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || len(hash) != sha256.Size {
		t.Fatalf("invalid ticket output")
	}
	digest := sha256.Sum256([]byte(raw))
	if string(hash) != string(digest[:]) {
		t.Fatal("stored ticket hash does not match opaque token")
	}
	raw2, hash2, err := randomTicket()
	if err != nil {
		t.Fatal(err)
	}
	if raw == raw2 || string(hash) == string(hash2) {
		t.Fatal("tickets must be unique")
	}
}
