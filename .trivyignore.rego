# Trivy ignore policy for blockyard container images.
#
# Rule-by-package, not by CVE ID, so future CVEs in the same
# package silence automatically on every base rebuild.

package trivy

import rego.v1

default ignore := false

# linux-libc-dev ships kernel header files pulled in transitively
# by rig's R installation. The CVEs describe running-kernel
# vulnerabilities — containers use the host kernel, not a kernel
# built from these headers, so they are not container-exploitable.
ignore if input.PkgName == "linux-libc-dev"
