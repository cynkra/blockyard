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

# review-after: 2026-07-25
# GNU tar archive-handling CVEs (45582 path-traversal via symlink
# + two-stage extraction; 5704 hidden file injection via crafted
# archives). Not exploitable in blockyard's current paths: bundle
# upload uses Go's stdlib archive/tar (no /usr/bin/tar exec), and
# R's own tar usage (e.g. install.packages extracting source
# tarballs) only runs after the user has supplied executable R
# code, which is already RCE-equivalent. Kept at CVE level on
# purpose — tar is a plausible legitimate exec target in future
# code, so a new, potentially different-class CVE in the same
# package should re-trigger triage rather than silence automatically.
ignore if input.VulnerabilityID in {
	"CVE-2025-45582",
	"CVE-2026-5704",
}

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

# review-after: 2026-07-25
# libde265 heap buffer overflows in HEIC decode (38949 targets
# display444as420; 38950 targets __interceptor_memcpy; 33164 and
# 33165 are additional 2026-series HEIC parser bugs in the same
# decode path). libde265-0 is pulled in transitively via libheif
# by the R graphics stack; no request-path consumer exposes it —
# the Go server doesn't decode images and blockyard's APIs don't
# accept HEIC. Reachable only from user R code that invokes HEIC
# decoding, which is already RCE-equivalent. Kept at CVE level
# because a future libde265 CVE may have a different shape (e.g.
# a parser bug on a path we overlooked) and deserves fresh triage.
ignore if input.VulnerabilityID in {
	"CVE-2024-38949",
	"CVE-2024-38950",
	"CVE-2026-33164",
	"CVE-2026-33165",
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

# review-after: 2026-07-25
# glibc CVEs in code paths that blockyard does not link or
# exercise: 4046 (iconv() DoS via specific charsets); 4437 (DNS
# response parsing in the resolver); 4438 (gethostbyaddr returning
# invalid hostname). All blockyard server binaries (blockyard,
# by-builder, by) build with CGO_ENABLED=0, so Go uses its
# pure-Go DNS resolver and stdlib unicode/transform machinery —
# glibc's getaddrinfo / gethostbyaddr / iconv are never linked
# into the request path. Publisher-supplied container image refs
# are pulled via the Docker client over the daemon socket
# (internal/backend/docker/docker.go ImagePull, internal/orchestrator
# /clone_docker.go), so registry hostname resolution happens in
# dockerd on the host, not in any process inside this image. The
# only in-image glibc DNS / iconv consumer is R inside the worker,
# which already runs untrusted user code (RCE-equivalent) and is
# gated by viewer-egress restrictions — escaping the worker via
# a MEDIUM glibc bug is strictly weaker than the in-worker
# capability already accepted. Kept at CVE level because a future
# glibc CVE may sit in a code path we add later (e.g. a CGO=1
# helper invoked on a request path) and deserves fresh triage.
ignore if input.VulnerabilityID in {
	"CVE-2026-4046",
	"CVE-2026-4437",
	"CVE-2026-4438",
}

# review-after: 2026-07-25
# dpkg-deb component vulnerability. dpkg-deb is invoked exactly
# once across our images, at build time, in docker/worker.Dockerfile
# to extract a pinned R .deb downloaded from cdn.posit.co. No
# runtime path feeds .deb files to dpkg-deb: no Go code execs
# `dpkg`/`dpkg-deb` (grep-confirmed across cmd/ and internal/),
# and rig (the R installation manager that runs at container
# start) handles tarballs from r-lib/rig releases, not .debs.
# Kept at CVE level because a future dpkg CVE could land in a
# subset we do invoke later (e.g. dpkg-query embedded in a hook)
# and should re-trigger triage.
ignore if input.VulnerabilityID == "CVE-2026-2219"

# review-after: 2026-07-25
# util-linux TOCTOU in the userspace mount(8) binary when setting
# up loop devices. The bug is in /usr/bin/mount, not in the
# mount(2) syscall. Blockyard never execs /usr/bin/mount: bwrap
# performs its bind / tmpfs setup via direct mount(2) syscalls
# (the --ro-bind / --bind / --tmpfs args in
# internal/backend/process/bwrap.go are bwrap CLI flags consumed
# by bwrap itself, not shell `mount` invocations), and the Docker
# backend uses the daemon's mount machinery (also kernel-direct).
# No losetup or `mount -o loop` invocation exists in the codebase.
# Kept at CVE level — a future util-linux CVE in a binary we do
# invoke (e.g. `mount` newly added to a preflight check) should
# re-trigger triage.
ignore if input.VulnerabilityID == "CVE-2026-27456"

# review-after: 2026-07-25
# libxpm4 X PixMap parser CVE. libxpm is pulled in transitively
# via libx11-6 (docker/worker.Dockerfile) by R's cairo runtime
# for X11 capability detection. Workers run R headless (no
# DISPLAY, PNG/PDF backends), and no request path parses .xpm
# files — the package is dead weight in the runtime image. Same
# structural argument as libpixman-1-0: only consumer is user
# R code, which is already RCE-equivalent. Kept at CVE level
# because libxpm4 is plausible to drop entirely once libx11's
# transitive depgraph is trimmed, at which point the rule should
# disappear rather than silently apply.
ignore if input.VulnerabilityID == "CVE-2026-4367"
