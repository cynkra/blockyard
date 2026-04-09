// seccomp-compile reads an OCI seccomp JSON profile, builds an
// in-memory libseccomp filter from its rules, and exports the
// compiled BPF blob to a file. The output is loaded by bwrap via
// `--seccomp <fd>` at worker spawn time.
//
// The program runs only in the variant Dockerfile build stage (the
// `seccomp-compiler` stage) and in the release workflow's seccomp-
// blob job. The runtime blockyard binary stays CGO-disabled; only
// this tool needs libseccomp.
//
// Invocation:
//
//	seccomp-compile -in <profile.json> -out <profile.bpf>
//
// The JSON schema understood here is the subset of the OCI format
// moby uses in profiles/seccomp/default.json: a top-level
// `defaultAction`, a list of `syscalls` rules with `names`, `action`,
// optional `args` matchers, and optional `includes.caps` /
// `excludes.caps` capability gating. Capability gating is flattened
// to unconditional rules — the build environment always has the
// cap, so an `includes.caps` guard is equivalent to an unconditional
// allow, and `excludes.caps` is equivalent to "don't emit this rule
// at all" (because a cap-restricted deny is a cap-allow for the
// CGO-enabled runtime, which isn't our threat model).
//
// Unknown syscalls (for example, arch-specific ones not present on
// the build host) are skipped silently — matching libseccomp's own
// runtime behavior. This is important for multi-arch builds: the
// amd64 compilation step produces a blob that omits arm64-specific
// syscalls it has no numbers for, and vice versa. Bwrap's runtime
// doesn't care about missing rules; it cares about well-formed BPF.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	seccomp "github.com/seccomp/libseccomp-golang"
)

// ociProfile mirrors the subset of the OCI seccomp JSON schema used
// by moby's default profile. Only the fields seccomp-compile needs.
type ociProfile struct {
	DefaultAction string       `json:"defaultAction"`
	Syscalls      []ociSyscall `json:"syscalls"`
}

type ociSyscall struct {
	Names    []string   `json:"names"`
	Action   string     `json:"action"`
	Args     []ociArg   `json:"args"`
	Includes ociFilters `json:"includes"`
	Excludes ociFilters `json:"excludes"`
}

type ociArg struct {
	Index    uint   `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo"`
	Op       string `json:"op"`
}

type ociFilters struct {
	Caps   []string `json:"caps"`
	Arches []string `json:"arches"`
}

// actionMap translates OCI action strings to libseccomp actions. A
// value of 0 for a missing action would silently become ActInvalid;
// the compile step errors on any unmapped name instead.
var actionMap = map[string]seccomp.ScmpAction{
	"SCMP_ACT_KILL":    seccomp.ActKillThread,
	"SCMP_ACT_KILL_PROCESS": seccomp.ActKillProcess,
	"SCMP_ACT_TRAP":    seccomp.ActTrap,
	"SCMP_ACT_ERRNO":   seccomp.ActErrno,
	"SCMP_ACT_TRACE":   seccomp.ActTrace,
	"SCMP_ACT_ALLOW":   seccomp.ActAllow,
	"SCMP_ACT_LOG":     seccomp.ActLog,
	"SCMP_ACT_NOTIFY":  seccomp.ActNotify,
}

// opMap translates OCI comparison operator names to libseccomp
// comparison ops. Only the operators moby's default profile uses
// are mapped; unknown operators are errors.
var opMap = map[string]seccomp.ScmpCompareOp{
	"SCMP_CMP_NE":        seccomp.CompareNotEqual,
	"SCMP_CMP_LT":        seccomp.CompareLess,
	"SCMP_CMP_LE":        seccomp.CompareLessOrEqual,
	"SCMP_CMP_EQ":        seccomp.CompareEqual,
	"SCMP_CMP_GE":        seccomp.CompareGreaterEqual,
	"SCMP_CMP_GT":        seccomp.CompareGreater,
	"SCMP_CMP_MASKED_EQ": seccomp.CompareMaskedEqual,
}

func main() {
	inPath := flag.String("in", "", "path to OCI seccomp JSON profile")
	outPath := flag.String("out", "", "path to write compiled BPF blob")
	flag.Parse()

	if *inPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: seccomp-compile -in <profile.json> -out <profile.bpf>")
		os.Exit(2)
	}

	if err := compile(*inPath, *outPath); err != nil {
		log.Fatalf("seccomp-compile: %v", err)
	}
}

func compile(inPath, outPath string) error {
	data, err := os.ReadFile(inPath) //nolint:gosec // build-time tool, input path is a config
	if err != nil {
		return fmt.Errorf("read %s: %w", inPath, err)
	}

	var profile ociProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return fmt.Errorf("parse %s: %w", inPath, err)
	}

	defaultAct, ok := actionMap[profile.DefaultAction]
	if !ok {
		return fmt.Errorf("unknown defaultAction %q", profile.DefaultAction)
	}

	filter, err := seccomp.NewFilter(defaultAct)
	if err != nil {
		return fmt.Errorf("new filter: %w", err)
	}
	defer filter.Release()

	for _, rule := range profile.Syscalls {
		// excludes.caps: the rule only applies when the process
		// does NOT have one of the listed caps. The build
		// environment always has caps, so we interpret this as
		// "don't emit this rule at all." Same shape in moby's
		// runtime — an excludes check on a cap the process holds
		// causes the rule to be skipped.
		if len(rule.Excludes.Caps) > 0 {
			continue
		}

		action, ok := actionMap[rule.Action]
		if !ok {
			return fmt.Errorf("unknown action %q", rule.Action)
		}

		// Flatten conditions once per rule. libseccomp requires the
		// conds slice to match the syscall being added, not the rule
		// being added, so we build it once and reuse it across the
		// rule's names.
		conds, err := buildConditions(rule.Args)
		if err != nil {
			return fmt.Errorf("build conditions: %w", err)
		}

		for _, name := range rule.Names {
			sc, err := seccomp.GetSyscallFromName(name)
			if err != nil {
				// Unknown syscall — skip silently (matches
				// libseccomp's own runtime behavior for arch-
				// specific or kernel-specific syscalls).
				continue
			}
			if len(conds) == 0 {
				if err := filter.AddRule(sc, action); err != nil {
					return fmt.Errorf("add rule %s: %w", name, err)
				}
				continue
			}
			if err := filter.AddRuleConditional(sc, action, conds); err != nil {
				return fmt.Errorf("add conditional rule %s: %w", name, err)
			}
		}
	}

	out, err := os.Create(outPath) //nolint:gosec // build-time tool, output path is a config
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer out.Close()

	if err := filter.ExportBPF(out); err != nil {
		return fmt.Errorf("export bpf: %w", err)
	}
	return nil
}

func buildConditions(args []ociArg) ([]seccomp.ScmpCondition, error) {
	if len(args) == 0 {
		return nil, nil
	}
	conds := make([]seccomp.ScmpCondition, 0, len(args))
	for _, a := range args {
		op, ok := opMap[a.Op]
		if !ok {
			return nil, fmt.Errorf("unknown comparison op %q", a.Op)
		}
		// MakeCondition is variadic — one value for most ops, two
		// for MASKED_EQ. The OCI JSON always provides both, but
		// libseccomp ignores the second for single-operand ops.
		cond, err := seccomp.MakeCondition(a.Index, op, a.Value, a.ValueTwo)
		if err != nil {
			return nil, fmt.Errorf("MakeCondition: %w", err)
		}
		conds = append(conds, cond)
	}
	return conds, nil
}
