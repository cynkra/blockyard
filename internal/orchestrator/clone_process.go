//go:build !minimal || process_backend

package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/units"
)

// executableFn is a test seam so process_integration_test.go can
// inject a pre-built blockyard binary path. In production this is
// os.Executable. Exported via export_test.go.
var executableFn = os.Executable

// processServerFactory implements ServerFactory by fork+exec-ing the
// same blockyard binary with BLOCKYARD_PASSIVE=1 and an alt-bind
// address from cfg.Update.AltBindRange.
type processServerFactory struct {
	cfg *config.Config
	log *slog.Logger
}

// NewProcessFactory constructs the process variant of ServerFactory.
func NewProcessFactory(cfg *config.Config) ServerFactory {
	return &processServerFactory{
		cfg: cfg,
		log: slog.Default(),
	}
}

// processInstance is the newServerInstance handle for a fork+exec-ed
// blockyard child. The alt bind is cached on the instance at creation
// time.
type processInstance struct {
	pid  int
	addr string // host:port — the new server's bind, rewritten for loopback polling
	cmd  *exec.Cmd
	log  *slog.Logger
}

func (p *processInstance) ID() string {
	return fmt.Sprintf("pid:%d", p.pid)
}

// Addr returns the loopback-accessible form of the alt bind. When the
// primary bind is a wildcard (0.0.0.0 or empty host), the instance
// rewrites it to 127.0.0.1 for polling — the orchestrator always runs
// on the same host as the new server, so loopback is always correct.
// Public hosts pass through unchanged; they're already loopback-
// reachable by their own name.
func (p *processInstance) Addr() string {
	return p.addr
}

// Kill sends SIGTERM, waits 10 seconds, and escalates to SIGKILL if
// the process is still alive. Best-effort; logs errors internally.
func (p *processInstance) Kill(ctx context.Context) {
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		p.log.Warn("process instance did not exit on SIGTERM, escalating",
			"pid", p.pid)
		_ = p.cmd.Process.Kill()
		<-done
	case <-ctx.Done():
		_ = p.cmd.Process.Kill()
		<-done
	}
}

// PreUpdate is a no-op for the process variant — the binary is
// already on disk and there's nothing to pull.
func (f *processServerFactory) PreUpdate(_ context.Context, _ string, sender task.Sender) error {
	sender.Write("Process variant: no image to pull; skipping prefetch.")
	return nil
}

// CurrentImageBase returns a stable placeholder. The process variant
// has no equivalent of an image name; the orchestrator's Update
// flow logs `<placeholder>:<version>` for consistency with the
// Docker variant.
func (f *processServerFactory) CurrentImageBase(_ context.Context) string {
	return "blockyard-process"
}

// CurrentImageTag returns the current server version. Used by the
// backup metadata so a rollback can restore the correct schema
// version even though the process variant doesn't support rollback
// via this orchestrator.
func (f *processServerFactory) CurrentImageTag(_ context.Context) string {
	if f.cfg != nil && f.cfg.Server.Backend == "process" {
		return "process"
	}
	return "process"
}

// SupportsRollback returns false — the previous version's binary is
// typically overwritten by an upgrade, and blockyard does not track
// where it might live. Operators restore manually by swapping the
// binary and restarting.
func (f *processServerFactory) SupportsRollback() bool { return false }

// CreateInstance picks a free alt port, fork+execs a new blockyard
// with BLOCKYARD_PASSIVE=1 and BLOCKYARD_SERVER_BIND set to the new
// bind, and returns a handle with the new server's address cached.
func (f *processServerFactory) CreateInstance(
	ctx context.Context,
	_ string,
	extraEnv []string,
	sender task.Sender,
) (newServerInstance, error) {
	primaryHost, _, err := net.SplitHostPort(f.cfg.Server.Bind)
	if err != nil {
		return nil, fmt.Errorf("parse server.bind %q: %w", f.cfg.Server.Bind, err)
	}

	altPort, err := f.pickAltPort(primaryHost)
	if err != nil {
		return nil, fmt.Errorf("pick alt bind port: %w", err)
	}
	altBind := net.JoinHostPort(primaryHost, strconv.Itoa(altPort))
	sender.Write(fmt.Sprintf("Alt bind: %s", altBind))

	self, err := executableFn()
	if err != nil {
		return nil, fmt.Errorf("resolve own executable: %w", err)
	}

	env := os.Environ()
	env = setEnv(env, "BLOCKYARD_PASSIVE", "1")
	env = setEnv(env, "BLOCKYARD_SERVER_BIND", altBind)
	for _, kv := range extraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env = setEnv(env, k, v)
		}
	}
	// Strip systemd-propagated vars that should not carry over.
	env = stripEnv(env, "INVOCATION_ID", "JOURNAL_STREAM")

	argv := []string{self}
	if f.cfg.ConfigPath != "" {
		argv = append(argv, "--config", f.cfg.ConfigPath)
	}

	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: argv[0] is os.Executable, argv[1:] is the validated config path
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // detach from old server's pgrp
		// No Pdeathsig — child must outlive parent.
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start new blockyard: %w", err)
	}
	// Reap the child in the background to avoid zombies. Kill waits
	// on the same cmd.Wait via a local goroutine; exec.Cmd tolerates
	// concurrent Wait calls when the first one returns.
	go func() { _ = cmd.Wait() }()

	f.log.Info("process orchestrator: started new instance",
		"pid", cmd.Process.Pid, "alt_bind", altBind)

	return &processInstance{
		pid:  cmd.Process.Pid,
		addr: loopbackAddrForPolling(altBind),
		cmd:  cmd,
		log:  f.log,
	}, nil
}

// pickAltPort probes the configured AltBindRange, returning the first
// port that can be bound on the primary host. The probe closes its
// listener immediately — the new blockyard reopens the port a moment
// later. TOCTOU window is small but non-zero; on failure the new
// server fails with "address already in use" and the operator
// retries.
func (f *processServerFactory) pickAltPort(host string) (int, error) {
	first, last, err := altBindRange(f.cfg)
	if err != nil {
		return 0, err
	}
	for port := first; port <= last; port++ {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		ln.Close()
		return port, nil
	}
	return 0, errors.New("no free ports in alt_bind_range")
}

// altBindRange returns the (first, last) port pair derived from
// cfg.Update.AltBindRange, falling back to the design default
// "8090-8099" when the field is empty or cfg.Update is nil.
func altBindRange(cfg *config.Config) (int, int, error) {
	const defaultRange = "8090-8099"
	raw := defaultRange
	if cfg.Update != nil && cfg.Update.AltBindRange != "" {
		raw = cfg.Update.AltBindRange
	}
	return units.ParsePortRange(raw)
}

// loopbackAddrForPolling rewrites a wildcard bind ("0.0.0.0:<p>",
// "[::]:<p>", or ":<p>") to "127.0.0.1:<p>" for the orchestrator's
// local /readyz polling. Non-wildcard binds (specific IPs or
// hostnames) pass through unchanged.
func loopbackAddrForPolling(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

// setEnv sets KEY=VALUE in a []string, replacing any existing KEY=...
// entry. Returns the updated slice.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// stripEnv removes the given keys from a KEY=VALUE slice.
func stripEnv(env []string, keys ...string) []string {
	out := env[:0]
	for _, kv := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
