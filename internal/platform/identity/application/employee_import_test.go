package application

import "testing"

func TestImportedAccountNameUsesFullPinyin(t *testing.T) {
	if got := importedAccountName("张 三"); got != "zhangsan" {
		t.Fatalf("importedAccountName() = %q, want zhangsan", got)
	}
	if got := importedAccountName("John Doe"); got != "johndoe" {
		t.Fatalf("importedAccountName() = %q, want johndoe", got)
	}
}
