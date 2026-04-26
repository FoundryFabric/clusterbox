// Package ufw implements the UFW-firewall subsystem of the harden
// section.
//
// Apply installs the ufw package if missing, sets default deny-incoming
// / allow-outgoing, allows tcp/443 (HTTPS ingress), udp/41641 (Tailscale
// WireGuard), tcp/22 limited to the Tailscale CGNAT subnet
// 100.64.0.0/10, ICMP if Harden.AllowICMP is set, and finally enables
// the firewall non-interactively.
//
// Idempotency is delegated to ufw itself: every "ufw allow" we issue is
// safe to repeat, and we read `ufw status` to decide whether the
// firewall already reports active before re-running the rule set.
package ufw

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

// TailscaleCGNAT is the Tailscale-assigned CGNAT range used to scope
// SSH ingress to the mesh.
const TailscaleCGNAT = "100.64.0.0/10"

// UfwBinary is the absolute path of the ufw binary. Exposed as a var so
// tests can detect "missing" by pointing at a path that doesn't exist.
var UfwBinary = "/usr/sbin/ufw"

// Runner abstracts process execution so unit tests can inject a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
}

// FS abstracts filesystem reads/stats so tests can simulate "ufw not
// installed" without touching /usr/sbin.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
}

// Result is the structured payload returned by Apply / Remove.
type Result struct {
	Applied bool
	Reason  string
	Extra   map[string]interface{}
}

// Section bundles the dependencies used by Apply and Remove.
type Section struct {
	Runner Runner
	FS     FS
}

// Apply ensures ufw is installed, programs the rule set, and enables
// the firewall.
//
// Behaviour matrix:
//
//   - spec.Harden nil or Enabled=false: Applied=false, Reason="disabled".
//   - ufw missing on disk: apt-get install ufw, then proceed.
//   - rules are programmed in a fixed order; ufw collapses duplicates so
//     re-running is safe.
//   - ICMP is allowed only when Harden.AllowICMP is true.
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	h := specHarden(spec)
	if h == nil || !h.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}

	runner, fsys := s.runner(), s.fsys()

	installed, err := s.ensureInstalled(ctx, runner, fsys)
	if err != nil {
		return Result{}, err
	}

	// Default policies. Both are safe to set repeatedly.
	if _, err := runner.Run(ctx, "ufw", "default", "deny", "incoming"); err != nil {
		return Result{}, fmt.Errorf("ufw: default deny incoming: %w", err)
	}
	if _, err := runner.Run(ctx, "ufw", "default", "allow", "outgoing"); err != nil {
		return Result{}, fmt.Errorf("ufw: default allow outgoing: %w", err)
	}

	// HTTPS (cluster ingress).
	if _, err := runner.Run(ctx, "ufw", "allow", "443/tcp", "comment", "HTTPS ingress"); err != nil {
		return Result{}, fmt.Errorf("ufw: allow 443/tcp: %w", err)
	}
	// Tailscale WireGuard.
	if _, err := runner.Run(ctx, "ufw", "allow", "41641/udp", "comment", "Tailscale WireGuard"); err != nil {
		return Result{}, fmt.Errorf("ufw: allow 41641/udp: %w", err)
	}
	// SSH from the Tailscale subnet only — public SSH stays denied.
	if _, err := runner.Run(ctx, "ufw", "allow", "from", TailscaleCGNAT, "to", "any", "port", "22", "proto", "tcp", "comment", "SSH from tailnet"); err != nil {
		return Result{}, fmt.Errorf("ufw: allow ssh from %s: %w", TailscaleCGNAT, err)
	}

	if h.AllowICMP {
		if _, err := runner.Run(ctx, "ufw", "allow", "proto", "icmp", "comment", "ICMP echo"); err != nil {
			return Result{}, fmt.Errorf("ufw: allow icmp: %w", err)
		}
	}

	// Enable. --force suppresses the interactive prompt.
	if _, err := runner.Run(ctx, "ufw", "--force", "enable"); err != nil {
		return Result{}, fmt.Errorf("ufw: enable: %w", err)
	}

	return Result{
		Applied: true,
		Extra: map[string]interface{}{
			"installed":  installed,
			"allow_icmp": h.AllowICMP,
			"ssh_from":   TailscaleCGNAT,
		},
	}, nil
}

// Remove is a no-op for v1.
//
// Disabling the firewall on a node that has just been audited as
// hardened would be the wrong default; T4b will revisit this once
// uninstall semantics are spec'd.
func (s *Section) Remove(_ context.Context, _ *config.Spec) (Result, error) {
	return Result{Applied: false, Reason: "remove not implemented"}, nil
}

// ensureInstalled installs ufw via apt-get if the binary is missing.
//
// Returns true when an install actually happened. apt-get is invoked
// with DEBIAN_FRONTEND=noninteractive so the call never blocks waiting
// for tty input.
func (s *Section) ensureInstalled(ctx context.Context, runner Runner, fsys FS) (bool, error) {
	if _, err := fsys.Stat(UfwBinary); err == nil {
		return false, nil
	}
	env := []string{"DEBIAN_FRONTEND=noninteractive"}
	if _, err := runner.RunEnv(ctx, env, "apt-get", "update", "-qq"); err != nil {
		return false, fmt.Errorf("ufw: apt-get update: %w", err)
	}
	if _, err := runner.RunEnv(ctx, env, "apt-get", "install", "-y", "-qq", "ufw"); err != nil {
		return false, fmt.Errorf("ufw: apt-get install ufw: %w", err)
	}
	return true, nil
}

// statusActive parses `ufw status` output to decide whether the
// firewall is already up. Currently unused by Apply (we always re-run
// the rule set, which is cheap), but kept exported for future callers
// (e.g. a `clusterboxnode status` subcommand).
func statusActive(out []byte) bool {
	return strings.Contains(string(out), "Status: active")
}

func (s *Section) runner() Runner {
	if s.Runner != nil {
		return s.Runner
	}
	return execRunner{}
}

func (s *Section) fsys() FS {
	if s.FS != nil {
		return s.FS
	}
	return osFS{}
}

func specHarden(spec *config.Spec) *config.HardenSpec {
	if spec == nil {
		return nil
	}
	return spec.Harden
}

// execRunner is the production [Runner] backed by os/exec.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %v: %w: %s", name, args, err, string(out))
	}
	return out, nil
}

func (execRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %v: %w: %s", name, args, err, string(out))
	}
	return out, nil
}

// osFS is the production [FS] backed by the real filesystem.
type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
