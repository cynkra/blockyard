# Makefile for blockyard auxiliary build tasks. The main build still
# runs via `go build`; this file hosts targets that involve moving
# files around or coordinating multiple steps (currently: seccomp
# profile regeneration from upstream moby).

.PHONY: regen-seccomp regen-seccomp-check

# regen-seccomp rebuilds internal/seccomp/blockyard-outer.json and
# internal/seccomp/blockyard-bwrap.json from the committed upstream
# and overlay sources. Run this after bumping moby in go.mod, or
# after hand-editing an overlay. CI runs regen-seccomp-check to
# detect drift.
#
# The upstream copy at internal/seccomp/upstream-default.json is
# vendored by hand: copy from
# $(go env GOPATH)/pkg/mod/github.com/moby/moby@<version>/oci/fixtures/default.json
# when bumping moby. This target does not refresh the upstream —
# it only runs the merge step.
regen-seccomp:
	go build -o bin/seccomp-merge ./cmd/seccomp-merge
	./bin/seccomp-merge \
		-upstream internal/seccomp/upstream-default.json \
		-overlay internal/seccomp/blockyard-outer-overlay.json \
		-out internal/seccomp/blockyard-outer.json
	./bin/seccomp-merge \
		-upstream internal/seccomp/upstream-default.json \
		-overlay internal/seccomp/blockyard-bwrap-overlay.json \
		-out internal/seccomp/blockyard-bwrap.json
	@echo "Regenerated blockyard-outer.json and blockyard-bwrap.json."

# regen-seccomp-check verifies the committed merged profiles match
# what regen-seccomp would produce. Used by CI to catch drift when
# moby is bumped without a paired regen-seccomp run.
regen-seccomp-check:
	$(MAKE) regen-seccomp
	@git diff --exit-code internal/seccomp/blockyard-outer.json internal/seccomp/blockyard-bwrap.json >/dev/null \
		|| { echo "seccomp profiles out of sync with overlay+upstream; run 'make regen-seccomp' and commit the result."; exit 1; }
