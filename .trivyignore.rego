# Trivy ignore policy for blockyard container images.
#
# Currently empty — issue #185 replaced the rocker/r-ver base for
# the server-process and server-everything images with a minimal
# ubuntu:24.04 + rig build, which drops the compiler toolchain and
# its transitive dependencies (binutils, libctf*, libgprofng*,
# linux-libc-dev). That eliminated the kernel-header and binutils
# CVE noise that previously required per-package suppressions
# here.
#
# If new no-upstream-fix CVEs reappear against runtime libraries
# we still ship (libpixman-1-0, libexpat1, tar, …), add narrow
# per-package rules below, each with a one-line rationale.
# Rule-by-package, not by CVE ID, so future CVEs in the same
# package silence automatically on every base rebuild.

package trivy

import rego.v1

default ignore := false
