package application

import "testing"

func TestValidateBackchannelLogoutURI(t *testing.T) {
	for _, value := range []string{"https://rp.example/logout", "https://rp.example/path"} {
		if err := ValidateBackchannelLogoutURI(value, false); err != nil {
			t.Fatalf("%s: %v", value, err)
		}
	}
	for _, value := range []string{"http://rp.example/logout", "https://rp.example/logout?x=1", "https://u:p@rp.example/logout", "javascript:alert(1)"} {
		if err := ValidateBackchannelLogoutURI(value, false); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	if err := ValidateBackchannelLogoutURI("http://127.0.0.1/logout", true); err != nil {
		t.Fatal(err)
	}
}
