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
# comment. Default cadence is 3 months from the time the rule is added
# or last re-reviewed — use a shorter date for fragile invariants; do
# not extend beyond 3 months without an explicit reason. The Review
# Reminders workflow (.github/workflows/review-reminders.yml) runs
# weekly and opens a tracker issue listing any expired dates — the
# issue is the forcing function, not a blocked PR. On re-review: if
# the argument still holds, bump the date; if not, remove or narrow
# the rule.
#
# For exec-reachable invariants ("Go code does not invoke tool X"), the
# `exec-guard` job in .github/workflows/ci.yml greps the Go source for
# `exec.Command("X", ...)` so the promise is enforced, not aspirational.
# Keep that list in sync with the package-level ignores in this file.

package trivy

import rego.v1

default ignore := false

# review-after: 2026-07-17
# linux-libc-dev ships kernel header files pulled in transitively
# by rig's R installation. The CVEs describe running-kernel
# vulnerabilities — containers use the host kernel, not a kernel
# built from these headers, so they are not container-exploitable.
ignore if input.PkgName == "linux-libc-dev"

# review-after: 2026-07-17
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
# is enforced by the exec-guard CI job; the "nothing links libbfd
# at runtime" half is residual risk — re-review if that changes.
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

# review-after: 2026-07-17
# GNU tar path-traversal via symlink + two-stage extraction. Not
# exploitable in blockyard's current paths: bundle upload uses Go's
# stdlib archive/tar (no /usr/bin/tar exec), and R's own tar usage
# (e.g. install.packages extracting source tarballs) only runs
# after the user has supplied executable R code, which is already
# RCE-equivalent. Kept at CVE level on purpose — tar is a plausible
# legitimate exec target in future code, so a new, potentially
# different-class CVE in the same package should re-trigger triage
# rather than silence automatically.
ignore if input.VulnerabilityID == "CVE-2025-45582"

# review-after: 2026-07-17
# libexpat algorithmic-complexity DoS: a ~2 MiB crafted XML causes
# dozens of seconds of processing. libexpat1 is a transitive dep
# (likely fontconfig), not linked on any request path in blockyard:
# the Go server has no encoding/xml usage, R's xml2 links libxml2
# explicitly installed alongside libexpat, not expat itself. The
# plausible in-image expat consumers (fontconfig reading fonts.conf)
# only parse developer-controlled config at process init, never
# attacker-supplied data. Kept at CVE level because the invariant
# is library-linkage — not grep-testable — so a future memory-
# corruption-class CVE in the same package should re-trigger
# triage rather than silence automatically.
ignore if input.VulnerabilityID == "CVE-2025-66382"

# review-after: 2026-07-17
# libde265 heap buffer overflows in HEIC decode (38949 targets
# display444as420; 38950 targets __interceptor_memcpy). libde265-0
# is pulled in transitively via libheif by the R graphics stack;
# no request-path consumer exposes it — the Go server doesn't
# decode images and blockyard's APIs don't accept HEIC. Reachable
# only from user R code that invokes HEIC decoding, which is
# already RCE-equivalent. Kept at CVE level because a future
# libde265 CVE may have a different shape (e.g. a parser bug on
# a path we overlooked) and deserves fresh triage.
ignore if input.VulnerabilityID in {
	"CVE-2024-38949",
	"CVE-2024-38950",
}

# review-after: 2026-07-17
# pixman is a 2D graphics rasterization library reached via cairo
# by R's plotting devices (png, pdf, svg). It operates on
# in-memory pixel buffers produced by R — no attacker-supplied
# serialized data is fed into it on any blockyard request path;
# the only consumer is user R code rendering plots, which is
# already RCE-equivalent. The current CVE (CVE-2023-37769) is
# additionally in the pixman stress-test binary that does not
# ship in the libpixman-1-0 runtime package at all. Kept at
# package level because pixman's role is structurally a
# rasterizer, not a data parser — re-review if any feature
# ever processes images server-side outside the R worker.
ignore if input.PkgName == "libpixman-1-0"
