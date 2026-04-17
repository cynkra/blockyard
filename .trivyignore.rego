# Trivy ignore policy for blockyard container images.
#
# Two rule styles are supported:
#
#   - Package-level (`input.PkgName == "..."` or `input.PkgName in {...}`):
#     silences all current and future CVEs in the package. Use only when
#     the reason for ignoring is structural — the package is not linked
#     or invoked on any code path that processes attacker-controlled
#     input — so the argument covers CVEs that haven't been filed yet.
#
#   - CVE-level (`input.VulnerabilityID == "CVE-..."`): silences one
#     finding. Use when the argument is specific to the reported bug,
#     so a future CVE in the same package forces fresh triage.
#
# Every `ignore if` rule must be preceded by a `# review-after: YYYY-MM-DD`
# comment. The `security-invariants` job in .github/workflows/ci.yml
# fails once any date is past, forcing a re-check of the invariant.
# On re-review: if the argument still holds, bump the date; if not,
# remove or narrow the rule.
#
# For exec-reachable invariants ("Go code does not invoke tool X"), the
# same CI job greps the Go source for `exec.Command("X", ...)` so the
# promise is enforced, not aspirational. Keep that list in sync with
# the ignored packages in this file.

package trivy

import rego.v1

default ignore := false

# review-after: 2027-04-17
# linux-libc-dev ships kernel header files pulled in transitively
# by rig's R installation. The CVEs describe running-kernel
# vulnerabilities — containers use the host kernel, not a kernel
# built from these headers, so they are not container-exploitable.
ignore if input.PkgName == "linux-libc-dev"

# review-after: 2027-04-17
# binutils and its transitive libraries are pulled in by rig to
# support the R source-build path (R CMD INSTALL compiles C/C++
# in user-supplied packages). They are a build toolchain, not a
# runtime parser of external input: nothing on the request path
# (Go server, R runtime, bubblewrap) links libbfd or invokes
# readelf/objdump against untrusted data. CVEs in this family
# are therefore only reachable by a malicious R package, which
# already has arbitrary code execution inside its worker — so
# they add no capability the threat model doesn't already accept.
# The "Go code does not exec these tools" half of the invariant
# is enforced by the security-invariants CI job; the "nothing
# links libbfd at runtime" half is residual risk — re-review if
# that changes.
ignore if input.PkgName in {
	"binutils",
	"binutils-common",
	"binutils-x86-64-linux-gnu",
	"libbinutils",
	"libctf0",
	"libctf-nobfd0",
	"libgprofng0",
	"libsframe1",
}
