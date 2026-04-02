package appname

import "testing"

func TestValidate(t *testing.T) {
	valid := []string{
		"a", "myapp", "my-app", "app1", "a1b2c3",
		"abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0", // 63 chars
	}
	for _, name := range valid {
		if err := Validate(name); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		why  string
	}{
		{"", "empty"},
		{"A", "uppercase"},
		{"MyApp", "mixed case"},
		{"1app", "starts with digit"},
		{"-app", "starts with hyphen"},
		{"app-", "ends with hyphen"},
		{"app name", "contains space"},
		{"app_name", "contains underscore"},
		{"app.name", "contains dot"},
		{string(make([]byte, 64)), "too long"},
	}
	for _, tc := range cases {
		if err := Validate(tc.name); err == nil {
			t.Errorf("Validate(%q) [%s] = nil, want error", tc.name, tc.why)
		}
	}
}
