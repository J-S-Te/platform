package application

import "testing"

func TestValidOptionalPersonID(t *testing.T) {
	for _, value := range []string{"", "PMS-U10086", "person:42"} {
		if !validOptionalPersonID(value) {
			t.Errorf("valid person id %q rejected", value)
		}
	}
	for _, value := range []string{" PMS-A", "PMS A", "PMS-A\n", "人员-一", "PMS-\u200bA", "PMS/A"} {
		if validOptionalPersonID(value) {
			t.Errorf("invalid person id %q accepted", value)
		}
	}
}
