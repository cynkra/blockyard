// Package apparmor exposes the shipped AppArmor profile as an
// embedded byte slice so the `by admin install-apparmor` CLI
// subcommand can drop it on operators' disks without requiring
// network access.
//
// The profile grants the `userns` permission narrowly to blockyard
// and its subprocesses so rootless bwrap can create its sandbox user
// namespace on hosts where `kernel.apparmor_restrict_unprivileged_userns=1`
// (Ubuntu 23.10+ default). Operators load it with
// `sudo apparmor_parser -r /etc/apparmor.d/blockyard`.
package apparmor

import _ "embed"

// Profile is the shipped AppArmor profile source. Embedded as bytes
// so the CLI can write it to disk; operators load it with
// `apparmor_parser -r`.
//
//go:embed blockyard
var Profile []byte

// DefaultInstallPath is where `apparmor_parser -r` expects the
// profile on Ubuntu/Debian systems.
const DefaultInstallPath = "/etc/apparmor.d/blockyard"
