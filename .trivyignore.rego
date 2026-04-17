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

# binutils and its transitive libraries are pulled in by rig to
# support the R source-build path (R CMD INSTALL compiles C/C++
# in user-supplied packages). They are a build toolchain, not a
# runtime parser of external input: nothing on the request path
# (Go server, R runtime, bubblewrap) links libbfd or invokes
# readelf/objdump against untrusted data. CVEs in this family
# are therefore only reachable by a malicious R package, which
# already has arbitrary code execution inside its worker — so
# they add no capability the threat model doesn't already accept.
# Re-review if anything on the request path starts feeding
# untrusted binaries to these tools or links libbfd directly.
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
