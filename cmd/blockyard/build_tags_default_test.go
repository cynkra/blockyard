//go:build !minimal

package main

import (
	"sort"
	"testing"
)

// TestDefaultVariantFactories verifies the default (no-tags) build
// registers both docker and process factories — this is the
// "everything" variant that `go build` without tags produces, and
// the basis of the blockyard:<v> image.
func TestDefaultVariantFactories(t *testing.T) {
	got := availableBackends()
	want := []string{"docker", "process"}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("availableBackends() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("availableBackends()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
