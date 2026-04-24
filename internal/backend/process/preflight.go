package process

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/preflight"
)

// RunPreflight verifies the process backend prerequisites. Called by
// (*ProcessBackend).Preflight() with the full config so the egress
// probe can read Redis/vault/database addresses and the resource-
// limit check can read server-level defaults.
//
// Check ordering matters: bwrap/R/userns are prerequisites for
// checkBwrapHostUIDMapping (it spawns bwrap), and that check is a
// prerequisite for checkWorkerEgress (which also spawns bwrap and
// whose results are meaningful only if the host UID mapping is
// effective). If a prerequisite fails we still run the later checks
// — they'll fail too, and emitting all failures at once is more
// useful than bailing at the first.
//
// cgroups is the optional cgroup-v2 delegation manager; nil is safe
// and tests pass nil directly. The manager feeds checkCgroupDelegation
// and the worker-egress probe's cgroup enrollment.
func RunPreflight(cfg *config.ProcessConfig, fullCfg *config.Config, cgroups *cgroupManager) *preflight.Report {
	r := &preflight.Report{RanAt: time.Now().UTC()}
	r.Add(checkBwrap(cfg))
	r.Add(checkRBinary(cfg))
	r.Add(checkRigVersions())
	r.Add(checkUserNamespaces())
	r.Add(checkPortRange(cfg))
	r.Add(checkResourceLimits(&fullCfg.Server))
	r.Add(checkSeccompProfile(cfg))
	r.Add(checkBwrapHostUIDMapping(cfg))
	r.Add(checkCloudMetadataReachable(cfg))
	r.Add(preflight.CheckRedisAuth(fullCfg.Redis))
	r.Add(checkCgroupDelegation(cgroups))
	r.Add(checkWorkerEgress(cfg, fullCfg, cgroups))
	return r
}

// checkSeccompProfile verifies the configured seccomp profile path
// exists and is a readable regular file. Empty path is valid —
// seccomp is optional in native mode, phase 3-7 treats it as
// such, and phase 3-8's containerized image sets a sensible
// default via BLOCKYARD_PROCESS_SECCOMP_PROFILE, so operators on
// bare-metal are free to omit it.
//
// Catches the "operator set the path but the file is missing or
// unreadable" footgun at startup instead of at first worker spawn.
func checkSeccompProfile(cfg *config.ProcessConfig) preflight.Result {
	const name = "seccomp_profile"
	if cfg.SeccompProfile == "" {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityOK,
			Message:  "no seccomp profile configured (optional)",
			Category: "process",
		}
	}
	info, err := os.Stat(cfg.SeccompProfile)
	if err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message: fmt.Sprintf(
				"seccomp profile %q: %v. "+
					"Run `by admin install-seccomp` or extract from the process-backend image.",
				cfg.SeccompProfile, err),
			Category: "process",
		}
	}
	if !info.Mode().IsRegular() {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("seccomp profile %q is not a regular file", cfg.SeccompProfile),
			Category: "process",
		}
	}
	if info.Size() == 0 {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("seccomp profile %q is empty", cfg.SeccompProfile),
			Category: "process",
		}
	}
	// Deeper BPF validity is checked by libseccomp when bwrap loads
	// the blob; preflight just confirms the file exists and is
	// readable.
	return preflight.Result{
		Name:     name,
		Severity: preflight.SeverityOK,
		Message:  fmt.Sprintf("seccomp profile readable (%d bytes)", info.Size()),
		Category: "process",
	}
}

func checkBwrap(cfg *config.ProcessConfig) preflight.Result {
	if _, err := exec.LookPath(cfg.BwrapPath); err != nil {
		return preflight.Result{
			Name:     "bwrap_available",
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("bwrap not found at %q", cfg.BwrapPath),
			Category: "process",
		}
	}
	out, err := exec.Command(cfg.BwrapPath, "--version").CombinedOutput() //nolint:gosec // G204: validated config path
	if err != nil {
		return preflight.Result{
			Name:     "bwrap_available",
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("bwrap --version failed: %v", err),
			Category: "process",
		}
	}
	return preflight.Result{
		Name:     "bwrap_available",
		Severity: preflight.SeverityOK,
		Message:  fmt.Sprintf("bwrap version: %s", strings.TrimSpace(string(out))),
		Category: "process",
	}
}

func checkRBinary(cfg *config.ProcessConfig) preflight.Result {
	if _, err := exec.LookPath(cfg.RPath); err != nil {
		return preflight.Result{
			Name:     "r_binary",
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("R not found at %q", cfg.RPath),
			Category: "process",
		}
	}
	return preflight.Result{
		Name:     "r_binary",
		Severity: preflight.SeverityOK,
		Message:  "R binary found",
		Category: "process",
	}
}

func checkRigVersions() preflight.Result {
	versions := InstalledRVersions()
	if len(versions) == 0 {
		return preflight.Result{
			Name:     "rig_versions",
			Severity: preflight.SeverityInfo,
			Message:  "no rig-managed R versions found in /opt/R",
			Category: "process",
		}
	}
	return preflight.Result{
		Name:     "rig_versions",
		Severity: preflight.SeverityOK,
		Message:  fmt.Sprintf("rig-managed R versions: %s", strings.Join(versions, ", ")),
		Category: "process",
	}
}

func checkUserNamespaces() preflight.Result {
	return checkUserNamespacesAt("/proc/sys/kernel/unprivileged_userns_clone")
}

// checkUserNamespacesAt is checkUserNamespaces with an injectable
// sysctl path so tests can run against fixture files.
func checkUserNamespacesAt(path string) preflight.Result {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is hardcoded in prod, fixture in tests
	if err != nil {
		// File doesn't exist — kernel allows unprivileged userns by default.
		return preflight.Result{
			Name:     "user_namespaces",
			Severity: preflight.SeverityOK,
			Message:  "unprivileged user namespaces available (sysctl absent, default allow)",
			Category: "process",
		}
	}
	if strings.TrimSpace(string(data)) == "0" {
		return preflight.Result{
			Name:     "user_namespaces",
			Severity: preflight.SeverityError,
			Message:  "unprivileged user namespaces disabled (kernel.unprivileged_userns_clone = 0); required for bwrap --unshare-user",
			Category: "process",
		}
	}
	return preflight.Result{
		Name:     "user_namespaces",
		Severity: preflight.SeverityOK,
		Message:  "unprivileged user namespaces enabled",
		Category: "process",
	}
}

// checkBwrapHostUIDMapping verifies that the spawn pipeline produces a
// sandboxed child whose kuid/kgid in init_userns equal the worker's
// host UID/GID — the precondition for iptables `--uid-owner $uid` /
// `--gid-owner $gid` rules to match worker traffic (decision #5).
//
// The mechanism is in bwrapSysProcAttr: when blockyard runs as root,
// the forked child calls setgid(gid)+setuid(uid) before exec(bwrap),
// so bwrap sees caller_uid == sandbox_uid and writes an identity
// uid_map (`uid uid 1`). The sandboxed child's kuid in init_userns is
// therefore `uid`. This check spawns a bwrap probe through the same
// helper and reads /proc/<bwrap-pid>/status to confirm — with an
// identity uid_map, the bwrap process itself is already at (uid, gid),
// and the check can read the parent pid directly without chasing the
// sandboxed grandchild through --info-fd.
//
// When blockyard is non-root, setuid(W) is rejected by the kernel, so
// bwrap's uid_map still maps sandbox_uid to blockyard's own uid. The
// `-m owner` mechanism is inherently inapplicable in that mode (not
// broken), and blocking startup was wrong — operators reach layer 6
// via cgroup-v2 delegation instead. The non-root branch reports Info
// with the alternatives; checkCgroupDelegation reports the cgroup
// path availability.
func checkBwrapHostUIDMapping(cfg *config.ProcessConfig) preflight.Result {
	const name = "bwrap_host_uid_mapping"

	if os.Getuid() != 0 {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityInfo,
			Message: "non-root blockyard cannot produce per-worker host kuids " +
				"via fork+setuid, so `iptables -m owner --uid-owner` rules do " +
				"not match worker traffic. This is inherent to non-root mode, " +
				"not a failure: workers still have filesystem, PID, capability, " +
				"seccomp, and in-sandbox UID isolation (layers 1-5), and " +
				"per-worker egress (layer 6) is available via cgroup-v2 " +
				"delegation — see cgroup_delegation. Alternatives: run as root " +
				"(containerized deployment) for the `-m owner` path, or use the " +
				"Docker backend for per-worker network namespaces.",
			Category: "process",
		}
	}

	// Probe UID/GID — must be distinct from blockyard's own (0). The
	// worker UID range start is a safe choice: it matches the real
	// worker mapping we care about.
	probeUID := cfg.WorkerUIDStart
	probeGID := cfg.WorkerGID

	// The bwrap monitor is itself (uid, gid) after our fork+setuid, so
	// we can poll /proc/<bwrap-pid>/status directly — no need to chase
	// the sandboxed grandchild through --info-fd. A long-enough sleep
	// keeps the monitor alive past the read.
	args := []string{
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid", "--unshare-user", "--unshare-uts",
		"--uid", strconv.Itoa(probeUID),
		"--gid", strconv.Itoa(probeGID),
		"--die-with-parent", "--new-session",
		"--cap-drop", "ALL",
		"--", "/bin/sleep", "3",
	}
	prog, argv, err := bwrapExecSpec(cfg.BwrapPath, probeUID, probeGID, args)
	if err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("bwrap-exec shim unavailable: %v", err),
			Category: "process",
		}
	}
	cmd := exec.Command(prog, argv...) //nolint:gosec // G204
	cmd.SysProcAttr = bwrapSysProcAttr()
	if err := cmd.Start(); err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("failed to spawn bwrap probe: %v", err),
			Category: "process",
		}
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Poll until the bwrap monitor's Uid/Gid settle at (probeUID, probeGID).
	// The bwrap-exec shim setuid+setgid's into (probeUID, probeGID)
	// before exec(bwrap), so this is usually already true on the first
	// read — but we poll in case the kernel hasn't scheduled the
	// exec'd process yet.
	var uidLine, gidLine string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid))
		if err == nil {
			var curUID, curGID string
			for _, line := range strings.Split(string(data), "\n") {
				switch {
				case strings.HasPrefix(line, "Uid:"):
					curUID = line
				case strings.HasPrefix(line, "Gid:"):
					curGID = line
				}
			}
			if curUID != "" && curGID != "" {
				hostUID, _ := parseStatusUID(curUID)
				hostGID, _ := parseStatusUID(curGID)
				uidLine = curUID
				gidLine = curGID
				if hostUID == probeUID && hostGID == probeGID {
					break
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if uidLine == "" || gidLine == "" {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  "bwrap probe exited before /proc could be read",
			Category: "process",
		}
	}

	realHostUID, err := parseStatusUID(uidLine)
	if err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("could not parse /proc/<pid>/status Uid line %q: %v", uidLine, err),
			Category: "process",
		}
	}
	realHostGID, err := parseStatusUID(gidLine)
	if err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message:  fmt.Sprintf("could not parse /proc/<pid>/status Gid line %q: %v", gidLine, err),
			Category: "process",
		}
	}

	if realHostUID != probeUID || realHostGID != probeGID {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityError,
			Message: fmt.Sprintf(
				"bwrap host-side identity mismatch: requested uid=%d gid=%d, "+
					"host /proc sees uid=%d gid=%d. This should not happen when "+
					"blockyard runs as root — the spawn pipeline should fork+setuid "+
					"before exec(bwrap). Investigate bwrapSysProcAttr wiring.",
				probeUID, probeGID, realHostUID, realHostGID,
			),
			Category: "process",
		}
	}
	return preflight.Result{
		Name:     name,
		Severity: preflight.SeverityOK,
		Message:  fmt.Sprintf("bwrap --uid/--gid are host-effective (child host uid=%d gid=%d)", realHostUID, realHostGID),
		Category: "process",
	}
}

// parseStatusUID extracts the first numeric field from a
// /proc/<pid>/status Uid: or Gid: line (the "real" id).
//
//	Uid:\t1000\t1000\t1000\t1000
func parseStatusUID(line string) (int, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("too few fields")
	}
	return strconv.Atoi(fields[1])
}

// checkResourceLimits warns when default_memory_limit or default_cpu_limit
// are set but the process backend cannot enforce them (decision #6).
// The fields live in [server] (not [docker]) so the same TOML works
// for Docker and a future k8s backend, but the process backend silently
// ignoring them would be a footgun. The warning makes the gap explicit.
func checkResourceLimits(srvCfg *config.ServerConfig) preflight.Result {
	var unset []string
	if srvCfg.DefaultMemoryLimit != "" {
		unset = append(unset, fmt.Sprintf("default_memory_limit=%q", srvCfg.DefaultMemoryLimit))
	}
	if srvCfg.DefaultCPULimit != 0 {
		unset = append(unset, fmt.Sprintf("default_cpu_limit=%v", srvCfg.DefaultCPULimit))
	}
	if len(unset) == 0 {
		return preflight.Result{
			Name:     "resource_limits",
			Severity: preflight.SeverityOK,
			Message:  "no per-worker resource limits configured",
			Category: "process",
		}
	}
	return preflight.Result{
		Name:     "resource_limits",
		Severity: preflight.SeverityWarning,
		Message: fmt.Sprintf(
			"process backend does not enforce per-worker resource limits; ignoring %s. "+
				"Use the Docker backend if you need cgroup-enforced limits.",
			strings.Join(unset, ", "),
		),
		Category: "process",
	}
}

func checkPortRange(cfg *config.ProcessConfig) preflight.Result {
	portCount := cfg.PortRangeEnd - cfg.PortRangeStart + 1
	if portCount < 10 {
		return preflight.Result{
			Name:     "port_range",
			Severity: preflight.SeverityWarning,
			Message:  fmt.Sprintf("port range only has %d ports; consider widening [process] port_range_start/port_range_end", portCount),
			Category: "process",
		}
	}
	return preflight.Result{
		Name:     "port_range",
		Severity: preflight.SeverityOK,
		Message:  fmt.Sprintf("port range: %d ports available", portCount),
		Category: "process",
	}
}

// checkWorkerEgress verifies that workers cannot reach sensitive
// network endpoints. It spawns the blockyard binary in `probe` mode
// inside a bwrap sandbox configured exactly like a real worker — same
// UID, same GID, same namespace flags — and asks it to TCP-connect
// to a list of targets. Any successful connection from inside the
// sandbox means a real worker would also succeed, indicating the
// operator's egress firewall is missing or misconfigured.
//
// Targets:
//   - 169.254.169.254:80 (cloud metadata) — always probed; ERROR if
//     reachable since there is no legitimate reason for a worker to
//     read instance credentials.
//   - Redis address (if configured) — WARNING if reachable.
//   - vault address (if configured) — WARNING if reachable.
//   - Database TCP address (if not SQLite) — WARNING if reachable.
//
// The probe binary is the same blockyard binary, invoked with
// `blockyard probe --tcp host:port`. It exits 0 on successful TCP
// connect, 1 on failure. No external tools required.
func checkWorkerEgress(cfg *config.ProcessConfig, fullCfg *config.Config, cgroups *cgroupManager) preflight.Result {
	type target struct {
		name     string
		addr     string
		critical bool // true → ERROR if reachable; false → WARNING
	}
	targets := []target{
		{name: "cloud_metadata", addr: "169.254.169.254:80", critical: true},
	}
	if fullCfg.Redis != nil && fullCfg.Redis.URL != "" {
		if hp := preflight.TCPAddrFromRedisURL(fullCfg.Redis.URL); hp != "" {
			targets = append(targets, target{name: "redis", addr: hp})
		}
	}
	if fullCfg.Vault != nil && fullCfg.Vault.Address != "" {
		if hp := preflight.TCPAddrFromHTTPURL(fullCfg.Vault.Address); hp != "" {
			targets = append(targets, target{name: "vault", addr: hp})
		}
	}
	if hp := preflight.TCPAddrFromDBConfig(fullCfg.Database); hp != "" {
		targets = append(targets, target{name: "database", addr: hp})
	}

	// Use the start of the worker UID range as the probe UID. Preflight
	// runs at startup before any worker spawns, so the allocator state
	// is irrelevant — there's nothing to collide with.
	probeUID := cfg.WorkerUIDStart
	probeGID := cfg.WorkerGID

	var reachable, blocked []string
	var critical bool
	for _, t := range targets {
		if probeReachableFn(cfg, cgroups, probeUID, probeGID, t.addr) {
			reachable = append(reachable, fmt.Sprintf("%s (%s)", t.name, t.addr))
			if t.critical {
				critical = true
			}
		} else {
			blocked = append(blocked, t.name)
		}
	}

	if len(reachable) == 0 {
		return preflight.Result{
			Name:     "worker_egress",
			Severity: preflight.SeverityOK,
			Message:  fmt.Sprintf("worker access to internal services is blocked: %s", strings.Join(blocked, ", ")),
			Category: "process",
		}
	}
	severity := preflight.SeverityWarning
	if critical {
		severity = preflight.SeverityError
	}
	return preflight.Result{
		Name:     "worker_egress",
		Severity: severity,
		Message: fmt.Sprintf(
			"workers can reach internal services: %s. "+
				"Install destination-scoped iptables rules, e.g. "+
				"`iptables -A OUTPUT -m owner --gid-owner %d -d <service-ip> -j REJECT` "+
				"for each internal endpoint. Do not use a blanket REJECT — "+
				"workers legitimately need the open internet. "+
				"See backends.md for details.",
			strings.Join(reachable, ", "), cfg.WorkerGID,
		),
		Category: "process",
	}
}

// probeReachableFn is a test seam: tests swap it for a pure
// predicate to exercise checkWorkerEgress's aggregation without
// spawning bwrap.
var probeReachableFn = probeReachable

// probeReachable spawns the blockyard binary in probe mode under the
// same bwrap config a worker would use, and reports whether the
// target TCP address is reachable. Returns false on probe error
// (treated as "not reachable" — fail-safe for the warning, not for
// security).
//
// cgroups (optional) enrolls the probe into the delegated
// `workers/` subcgroup before its first connect(), so operator
// `iptables -m cgroup --path workers` rules match the probe the
// same way they'd match a real worker. Without this, the probe
// stays in blockyard's own cgroup and would reach targets that
// real workers cannot. A bounded race exists between cmd.Start
// and the enroll write, but bwrap's namespace/mount setup
// (~10–50 ms) swamps the enroll write (~1 ms).
func probeReachable(cfg *config.ProcessConfig, cgroups *cgroupManager, uid, gid int, target string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	args := []string{
		"--unshare-pid", "--unshare-user", "--unshare-uts",
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--die-with-parent", "--new-session",
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
		"--chdir", "/tmp",
		"--cap-drop", "ALL",
		"--",
		self, "probe", "--tcp", target, "--timeout", "2s",
	}
	prog, argv, err := bwrapExecSpec(cfg.BwrapPath, uid, gid, args)
	if err != nil {
		return false
	}
	cmd := exec.Command(prog, argv...) //nolint:gosec // G204
	cmd.SysProcAttr = bwrapSysProcAttr()
	if err := cmd.Start(); err != nil {
		return false
	}
	cgroups.Enroll(cmd.Process.Pid) // no-op when delegation unavailable
	return cmd.Wait() == nil        // exit 0 = connect succeeded
}

// checkCloudMetadataReachable attempts a TCP connect to the link-local
// cloud metadata endpoint from blockyard's own process (not from
// inside a bwrap sandbox). Workers share the host network in the
// process backend, so if blockyard can reach the endpoint, so can
// every worker — and a compromised worker can steal the VM's IAM
// credentials. Reachable is treated as SeverityError prompting the
// operator to install a host-wide block rule or use a token-scoped
// metadata service (IMDSv2 / Workload Identity).
//
// Skipped when `[process] skip_metadata_check = true`. The escape
// hatch is for the rare deployment where blockyard legitimately needs
// metadata access (e.g. running on a VM whose IAM role is used by
// blockyard itself for S3 storage); operators who opt in also accept
// the worker-compromise implication.
func checkCloudMetadataReachable(cfg *config.ProcessConfig) preflight.Result {
	const name = "cloud_metadata"
	if cfg.SkipMetadataCheck {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityInfo,
			Message:  "cloud metadata check skipped by [process] skip_metadata_check",
			Category: "process",
		}
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.Dial("tcp", "169.254.169.254:80")
	if err != nil {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityOK,
			Message:  "cloud metadata endpoint not reachable from blockyard",
			Category: "process",
		}
	}
	_ = conn.Close()
	return preflight.Result{
		Name:     name,
		Severity: preflight.SeverityError,
		Message: "cloud metadata endpoint (169.254.169.254) is reachable from blockyard. " +
			"A compromised worker can steal this VM's IAM credentials. " +
			"Block it with `iptables -A OUTPUT -d 169.254.169.254 -j REJECT`, " +
			"enable IMDSv2 (EC2) or Workload Identity (GCP/AKS), " +
			"or run on a VM without an attached instance role. " +
			"Set [process] skip_metadata_check = true to suppress this check.",
		Category: "process",
	}
}

// checkCgroupDelegation reports whether cgroup-v2 delegation is
// available and the workers subcgroup was created. When available,
// also probes for the xt_cgroup netfilter module and escalates the
// severity to Warning if missing — operators installing
// `iptables -m cgroup --path` rules would otherwise hit a cryptic
// "No chain/target/match by that name" at rule-install time.
//
// nil cgroups is treated as "unavailable" so tests that construct a
// fake Preflight without initializing cgroup detection still get a
// well-formed result.
func checkCgroupDelegation(cgroups *cgroupManager) preflight.Result {
	const name = "cgroup_delegation"
	if cgroups == nil || cgroups.workersPath == "" {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityInfo,
			Message: "cgroup-v2 delegation unavailable. Per-worker egress " +
				"isolation via `iptables -m cgroup --path` is not available " +
				"on this host. Root deployments can use `iptables -m owner " +
				"--gid-owner` rules on the per-worker host kuids instead. " +
				"For non-root deployments wanting per-worker egress: enable " +
				"cgroup delegation (systemd: Delegate=yes on the unit) or " +
				"use the Docker backend.",
			Category: "process",
		}
	}
	xtCgroup := xtCgroupAvailable()
	cgRoot := filepath.Dir(cgroups.workersPath)
	cgMatchPath := strings.TrimPrefix(cgroups.workersPath, "/sys/fs/cgroup/")
	msg := fmt.Sprintf(
		"cgroup-v2 delegation available at %q; workers moved into %q. "+
			"Install a rule like `iptables -A OUTPUT -m cgroup --path %s -d <service-ip> -j REJECT` "+
			"to block worker access to internal services.",
		cgRoot, cgroups.workersPath, cgMatchPath,
	)
	if !xtCgroup {
		return preflight.Result{
			Name:     name,
			Severity: preflight.SeverityWarning,
			Message: msg + " WARNING: the xt_cgroup netfilter module does " +
				"not appear to be loaded (no match in /proc/net/ip_tables_matches); " +
				"`iptables -m cgroup` rules will fail to install. Run " +
				"`sudo modprobe xt_cgroup` or add it to /etc/modules-load.d/.",
			Category: "process",
		}
	}
	return preflight.Result{
		Name:     name,
		Severity: preflight.SeverityOK,
		Message:  msg,
		Category: "process",
	}
}

// xtCgroupAvailable reports whether the xt_cgroup netfilter match is
// loaded. /proc/net/ip_tables_matches lists builtin+loaded matches,
// one per line. Returns true on any read error so we don't emit a
// false warning on hosts where the file isn't accessible (rootless
// containers, odd /proc mounts).
func xtCgroupAvailable() bool {
	data, err := os.ReadFile("/proc/net/ip_tables_matches")
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "cgroup" {
			return true
		}
	}
	return false
}
