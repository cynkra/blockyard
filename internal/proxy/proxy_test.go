package proxy

import (
	"testing"
)

// --- stripAppPrefix unit tests ---

func TestStripAppPrefix(t *testing.T) {
	tests := []struct {
		path    string
		appName string
		want    string
	}{
		{"/app/myapp/", "myapp", "/"},
		{"/app/myapp/foo/bar", "myapp", "/foo/bar"},
		{"/app/myapp", "myapp", "/"},
		{"/app/myapp/foo", "myapp", "/foo"},
	}
	for _, tt := range tests {
		got := stripAppPrefix(tt.path, tt.appName)
		if got != tt.want {
			t.Errorf("stripAppPrefix(%q, %q) = %q, want %q",
				tt.path, tt.appName, got, tt.want)
		}
	}
}
