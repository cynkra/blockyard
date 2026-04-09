// Package build holds build-system tests that verify the variant
// matrix (see docs/design/v3/phase-3-8.md): a binary built with
// `-tags 'minimal,process_backend'` must not import the Docker SDK,
// and vice versa, so that variant images ship only the code they
// actually need.
package build_test

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenImport is a single "this package must not be in the dep
// graph" assertion for a named build variant.
type forbiddenImport struct {
	tags   string // -tags value, empty for default
	needle string // substring that must NOT appear in `go list -deps`
}

// TestVariantDependencyIsolation verifies the positive build-tag
// scheme actually keeps the two backends' dep graphs disjoint. Each
// variant is built from the repo root via `go list`; any regression
// that adds an untagged import pulling a backend into the wrong
// variant trips this test before it reaches CI.
//
// The default build (no tags) is not asserted here because it
// intentionally includes both backends (the "everything" variant).
// variant_builds_test.go covers the factory-registration side of
// the contract.
func TestVariantDependencyIsolation(t *testing.T) {
	cases := []forbiddenImport{
		{
			tags:   "minimal,process_backend",
			needle: "github.com/moby/moby",
		},
		{
			tags:   "minimal,process_backend",
			needle: "github.com/cynkra/blockyard/internal/backend/docker",
		},
		{
			tags:   "minimal,docker_backend",
			needle: "github.com/cynkra/blockyard/internal/backend/process",
		},
	}
	for _, tc := range cases {
		t.Run(tc.tags+"_no_"+tc.needle, func(t *testing.T) {
			out, err := exec.Command(
				"go", "list", "-deps",
				"-tags", tc.tags,
				"github.com/cynkra/blockyard/cmd/blockyard",
			).CombinedOutput()
			if err != nil {
				t.Fatalf("go list -deps -tags %q: %v\n%s", tc.tags, err, out)
			}
			if strings.Contains(string(out), tc.needle) {
				t.Errorf("variant %q dep graph contains forbidden import %q",
					tc.tags, tc.needle)
			}
		})
	}
}
