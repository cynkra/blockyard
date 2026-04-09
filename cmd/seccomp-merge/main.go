// seccomp-merge merges the vendored upstream moby seccomp profile
// with blockyard's overlay and writes the combined JSON file. The
// merged file is committed at internal/seccomp/blockyard-outer.json
// and embedded into the `by` binary via //go:embed so `by admin
// install-seccomp` can drop it on operators' disks.
//
// The overlay format is intentionally narrow — just a
// "prependRules" list that's inserted at the top of the upstream's
// "syscalls" array, before any cap-gated entries. This matches how
// seccomp evaluates rules in order: unconditional allows at the
// head win over later cap-restricted entries.
//
// Invocation:
//
//	seccomp-merge -upstream <path> -overlay <path> -out <path>
//
// Called from `make regen-seccomp`. CI runs this and fails if the
// committed blockyard-outer.json differs from the regenerated
// output, catching drift when moby is bumped in go.mod.
//
// Deliberately no CGO — this is a pure data transformation. Only
// cmd/seccomp-compile needs libseccomp.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// seccompProfile is a lightweight structural representation of the
// OCI seccomp JSON schema. Enough fields to read, merge, and write;
// not a validator (cmd/seccomp-compile exercises the rules).
type seccompProfile struct {
	DefaultAction string            `json:"defaultAction"`
	ArchMap       []json.RawMessage `json:"archMap,omitempty"`
	Syscalls      []json.RawMessage `json:"syscalls"`
}

// overlay is the blockyard-specific patch on top of the upstream.
// Currently a prepend-only overlay; future fields (remove lists,
// replace lists) can be added here without breaking consumers.
type overlay struct {
	Description   string            `json:"description,omitempty"`
	PrependRules  []json.RawMessage `json:"prependRules"`
}

func main() {
	upstreamPath := flag.String("upstream", "", "path to upstream moby default.json")
	overlayPath := flag.String("overlay", "", "path to blockyard overlay")
	outPath := flag.String("out", "", "path to write merged profile (stdout if '-')")
	flag.Parse()

	if *upstreamPath == "" || *overlayPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: seccomp-merge -upstream <path> -overlay <path> -out <path>")
		os.Exit(2)
	}

	merged, err := mergeProfile(*upstreamPath, *overlayPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seccomp-merge: %v\n", err)
		os.Exit(1)
	}

	if *outPath == "-" {
		if _, err := os.Stdout.Write(merged); err != nil {
			fmt.Fprintf(os.Stderr, "seccomp-merge: stdout: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := os.WriteFile(*outPath, merged, 0o644); err != nil { //nolint:gosec // non-secret output
		fmt.Fprintf(os.Stderr, "seccomp-merge: write %s: %v\n", *outPath, err)
		os.Exit(1)
	}
}

// mergeProfile reads the upstream profile and overlay, prepends the
// overlay's rules to the upstream's syscalls, and returns the
// pretty-printed JSON bytes ready to commit.
func mergeProfile(upstreamPath, overlayPath string) ([]byte, error) {
	upstreamData, err := os.ReadFile(upstreamPath) //nolint:gosec // build-time tool reading from the repo
	if err != nil {
		return nil, fmt.Errorf("read upstream: %w", err)
	}
	overlayData, err := os.ReadFile(overlayPath) //nolint:gosec // same
	if err != nil {
		return nil, fmt.Errorf("read overlay: %w", err)
	}

	// Decode upstream into a raw-preserving form — we want to write
	// back the exact upstream bytes for everything except the
	// syscalls list, so the merged file is a minimal diff from the
	// committed upstream-default.json.
	var upstreamMap map[string]json.RawMessage
	if err := json.Unmarshal(upstreamData, &upstreamMap); err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}

	var upstreamSyscalls []json.RawMessage
	if raw, ok := upstreamMap["syscalls"]; ok {
		if err := json.Unmarshal(raw, &upstreamSyscalls); err != nil {
			return nil, fmt.Errorf("parse upstream syscalls: %w", err)
		}
	}

	var ov overlay
	if err := json.Unmarshal(overlayData, &ov); err != nil {
		return nil, fmt.Errorf("parse overlay: %w", err)
	}

	// Prepend the overlay's rules to the upstream's syscalls so the
	// unconditional allows win rule-order evaluation.
	merged := make([]json.RawMessage, 0, len(ov.PrependRules)+len(upstreamSyscalls))
	merged = append(merged, ov.PrependRules...)
	merged = append(merged, upstreamSyscalls...)

	mergedSyscalls, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal merged syscalls: %w", err)
	}
	upstreamMap["syscalls"] = mergedSyscalls

	// Final output: pretty-printed for human review. The indent is
	// tabs to match the upstream's own formatting (moby uses tabs).
	return json.MarshalIndent(upstreamMap, "", "\t")
}
