# Trivy ignore policy for blockyard container images.
#
# Rationale for suppressing linux-libc-dev: the package ships only
# kernel header files (/usr/include/linux/*, /usr/include/asm/*) as
# a transitive dep of libc6-dev -> g++ in the rocker/r-ver base
# used by server-everything and server-process. The CVEs describe
# running-kernel vulnerabilities — containers use the host kernel,
# not a kernel built from these headers, so these CVEs are not
# container-exploitable.
#
# We intentionally keep the package because purging it cascades
# into libc6-dev / g++ / gfortran removal and breaks R package
# source compilation. See issue #185 for the planned migration off
# rocker, which makes this policy obsolete.
#
# Rule-by-package rather than rule-by-CVE-ID so new kernel-header
# CVEs are silenced automatically on every base bump — no list of
# CVE IDs to maintain.

package trivy

import rego.v1

default ignore := false

ignore if input.PkgName == "linux-libc-dev"
