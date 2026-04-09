package seccomp

import (
	"encoding/json"
	"os"
	"testing"
)

type syscallRule struct {
	Names    []string          `json:"names"`
	Action   string            `json:"action"`
	Args     []json.RawMessage `json:"args"`
	Includes struct {
		Caps []string `json:"caps"`
	} `json:"includes"`
}

type profile struct {
	DefaultAction string        `json:"defaultAction"`
	Syscalls      []syscallRule `json:"syscalls"`
}

// TestOuterProfileAllowsUserNS verifies that the committed
// blockyard-outer.json contains an unconditional allow for the
// user-namespace-creation syscalls, placed before any cap-gated
// rule referencing them. Regression guard: an accidental merge of
// a new upstream that happens to reorder these rules would
// re-introduce the EPERM bwrap failure phase 3-8 was built to
// fix.
func TestOuterProfileAllowsUserNS(t *testing.T) {
	var p profile
	if err := json.Unmarshal(Outer, &p); err != nil {
		t.Fatalf("parse embedded profile: %v", err)
	}
	wantSyscalls := map[string]bool{
		"clone":   true,
		"clone3":  true,
		"unshare": true,
		"setns":   true,
	}
	// Walk rules in order. The first rule for each relevant syscall
	// must be an unconditional (no args, no caps, no excludes)
	// allow — that's what lets an unprivileged process create a
	// user namespace.
	allowed := map[string]bool{}
	for _, r := range p.Syscalls {
		// Once we see a cap-gated rule naming one of our syscalls,
		// stop scanning — the unconditional allow above (if any)
		// would already have populated `allowed`.
		if len(r.Includes.Caps) > 0 {
			for _, n := range r.Names {
				if wantSyscalls[n] && !allowed[n] {
					t.Errorf("syscall %q first appears under cap-gated rule (caps=%v); expected unconditional allow before it",
						n, r.Includes.Caps)
				}
			}
			continue
		}
		if r.Action != "SCMP_ACT_ALLOW" || len(r.Args) > 0 {
			continue
		}
		for _, n := range r.Names {
			if wantSyscalls[n] {
				allowed[n] = true
			}
		}
	}
	for name := range wantSyscalls {
		if !allowed[name] {
			t.Errorf("profile does not unconditionally allow %q", name)
		}
	}
}

// TestEmbedMatchesDisk verifies the //go:embed copy of the profile
// matches the on-disk committed file byte-for-byte. Without this,
// a manual edit of the JSON without a rebuild could lead to the
// `by admin install-seccomp` CLI shipping a stale profile.
func TestEmbedMatchesDisk(t *testing.T) {
	disk, err := os.ReadFile("blockyard-outer.json")
	if err != nil {
		t.Fatalf("read on-disk profile: %v", err)
	}
	if string(Outer) != string(disk) {
		t.Errorf("embedded profile differs from on-disk blockyard-outer.json; run `make regen-seccomp`")
	}
}
