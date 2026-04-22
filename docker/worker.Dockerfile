# Worker image for the Docker backend (issue #191). Replaces the
# upstream rocker/r-ver:<v> default with a slim ubuntu + single-
# version R foundation, mirroring server-process.Dockerfile (#185).
#
# Rocker ships a full compiler toolchain (binutils, g++, gfortran)
# and -dev headers at runtime — convenient for interactive research,
# dead weight for a runtime that only loads compiled R packages.
# Dropping the toolchain shrinks the image and removes build-time
# binaries from the attack surface the container boundary has to
# contain, but this isn't "hardening" in the sandbox / capability-
# drop / seccomp sense — runtime isolation is the operator's
# responsibility via `docker run` flags. Operators who need source
# builds or extra packages layer them on via FROM blockyard-worker:<v>.
#
# One image per R version. Built and pushed as
# ghcr.io/cynkra/blockyard-worker:<r-version> by worker-publish.yml
# (current + previous 4 minor releases). Operators on older or niche
# R versions can extend this image or bring their own via [docker]
# image.
#
# R itself comes from Posit Package Manager's (cdn.posit.co) per-R-
# version .deb. That deb is dynamically linked and ships libR.so —
# important, because pak pulls binary R packages from p3m.dev that
# are NEEDED against libR.so and fail to dyn.load otherwise. Earlier
# iterations of this Dockerfile used r-hub's glibc .deb, which is
# statically linked and therefore incompatible with the p3m binaries.
#
# The PPM deb declares `g++ gcc gfortran make lib*-dev` as apt
# Depends — for users who will compile packages from source. We
# explicitly don't want that here (hardening goal), so the install
# goes through `dpkg-deb -x` instead of `apt install`: the files
# land at /opt/R/<ver>/ without dragging in the toolchain. The
# runtime shared libs libR.so actually NEEDs are enumerated in the
# apt list below.
#
# No blockyard binary, no bwrap/seccomp profile, no entrypoint —
# the Docker backend supplies the full Cmd at spawn time.
FROM ubuntu:24.04

ARG R_VERSION=4.4.3
ARG TARGETARCH

# Runtime libraries. Enumerates every NEEDED tag reachable from
# /opt/R/<ver>/lib/R/{lib,library/<pkg>/libs} — anything R's default
# packages (base/stats/utils/methods/graphics/grDevices/datasets)
# dyn.load at startup. That's a superset of what PPM's deb declares
# as runtime Depends (we skip the -dev and compiler entries). Second
# block covers the apt list in server-process.Dockerfile so process-
# backend and docker-backend workers see the same shared libraries —
# libuv1 in particular is required by fs (#264) and every Shiny app
# depends on fs transitively via shiny → bslib → sass → fs.
RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        libblas3 \
        libbz2-1.0 \
        libdeflate0 \
        libglib2.0-0t64 \
        libgomp1 \
        libicu74 \
        libjpeg8 \
        liblapack3 \
        liblzma5 \
        libpcre2-8-0 \
        libpng16-16t64 \
        libreadline8t64 \
        libtiff6 \
        libtirpc3t64 \
        libx11-6 \
        libxt6t64 \
        zlib1g \
        libcairo2 \
        libcurl4t64 \
        liblz4-1 \
        libmariadb3 \
        libodbc2 \
        libpango-1.0-0 \
        libpangocairo-1.0-0 \
        libpq5 \
        libsqlite3-0 \
        libssl3t64 \
        libuv1 \
        libxml2 \
        libzstd1 \
    && case "${TARGETARCH}" in \
        arm64) R_ARCH="arm64" ;; \
        amd64) R_ARCH="amd64" ;; \
        *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /tmp/r.deb \
        "https://cdn.posit.co/r/ubuntu-2404/pkgs/r-${R_VERSION}_1_${R_ARCH}.deb" \
    && dpkg-deb -x /tmp/r.deb / \
    && rm -f /tmp/r.deb \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf "/opt/R/${R_VERSION}/bin/R" /usr/local/bin/R \
    && ln -sf "/opt/R/${R_VERSION}/bin/Rscript" /usr/local/bin/Rscript \
    # Smoke: load every default package so any missing system lib
    # surfaces at image build time, not at first worker spawn.
    && Rscript -e 'invisible(lapply(c("stats","utils","graphics","grDevices","methods","datasets"), library, character.only=TRUE))'
