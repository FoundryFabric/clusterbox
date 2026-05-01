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
//
// When a Distro is provided and its ID is "flatcar", the UFW path is
// replaced with direct iptables calls and boot-persistence via a
// systemd unit.
package ufw

import (
	"context"
	_ "embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/distro"
)

//go:embed assets/iptables-restore.service
var iptablesRestoreService []byte

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
// installed" without touching /usr/sbin.  WriteFile supports the
// Flatcar path which persists iptables rules to disk.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	WriteFile(path string, data []byte, perm fs.FileMode) error
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
	// Distro, if non-nil, selects the OS-specific firewall path.
	// nil or distro.ID()=="ubuntu" use the standard UFW path.
	// distro.ID()=="flatcar" uses direct iptables rules.
	Distro distro.Distro
}

// Apply ensures the firewall is configured and active.
//
// Behaviour matrix:
//
//   - spec.Harden nil or Enabled=false: Applied=false, Reason="disabled".
//   - Distro is nil or "ubuntu": UFW path (install, program rules, enable).
//   - Distro is "flatcar": iptables path (idempotent -C/-A, persist to disk,
//     install systemd unit).
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	h := specHarden(spec)
	if h == nil || !h.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}

	if s.Distro != nil && s.Distro.ID() == "flatcar" {
		return s.applyFlatcar(ctx, h)
	}
	return s.applyUFW(ctx, h)
}

// applyUFW is the existing Ubuntu/UFW path, unchanged.
func (s *Section) applyUFW(ctx context.Context, h *config.HardenSpec) (Result, error) {
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

// iptRule describes a single iptables rule as its chain and arguments.
type iptRule struct {
	chain string // e.g. "INPUT", "OUTPUT"
	args  []string
}

// flatcarRules returns the ordered list of iptables rules to apply.
// The DROP rule is always last in INPUT; ICMP is inserted just before DROP
// when allowICMP is true.
func flatcarRules(allowICMP bool) []iptRule {
	rules := []iptRule{
		// INPUT: allow HTTPS
		{chain: "INPUT", args: []string{"-p", "tcp", "--dport", "443", "-j", "ACCEPT"}},
		// INPUT: allow Tailscale WireGuard
		{chain: "INPUT", args: []string{"-p", "udp", "--dport", "41641", "-j", "ACCEPT"}},
		// INPUT: allow SSH from Tailscale CGNAT only
		{chain: "INPUT", args: []string{"-s", TailscaleCGNAT, "-p", "tcp", "--dport", "22", "-j", "ACCEPT"}},
	}
	if allowICMP {
		rules = append(rules, iptRule{chain: "INPUT", args: []string{"-p", "icmp", "-j", "ACCEPT"}})
	}
	// Default deny INPUT — MUST be last.
	rules = append(rules, iptRule{chain: "INPUT", args: []string{"-j", "DROP"}})
	// Default allow OUTPUT.
	rules = append(rules, iptRule{chain: "OUTPUT", args: []string{"-j", "ACCEPT"}})
	return rules
}

// applyFlatcar applies iptables rules on Flatcar Container Linux, persists
// them to /etc/iptables/rules.v4 via iptables-save, and installs a
// systemd unit for boot-time restoration.
func (s *Section) applyFlatcar(ctx context.Context, h *config.HardenSpec) (Result, error) {
	runner := s.runner()
	fsys := s.fsys()

	for _, r := range flatcarRules(h.AllowICMP) {
		checkArgs := append([]string{"-C", r.chain}, r.args...)
		if _, err := runner.Run(ctx, "iptables", checkArgs...); err != nil {
			// Rule not present — add it.
			addArgs := append([]string{"-A", r.chain}, r.args...)
			if _, err := runner.Run(ctx, "iptables", addArgs...); err != nil {
				return Result{}, fmt.Errorf("iptables: add rule %v: %w", r, err)
			}
		}
		// err == nil means rule already present; skip (idempotent).
	}

	// Persist rules.
	saved, err := runner.Run(ctx, "iptables-save")
	if err != nil {
		return Result{}, fmt.Errorf("iptables-save: %w", err)
	}
	if err := fsys.WriteFile("/etc/iptables/rules.v4", saved, 0o600); err != nil {
		return Result{}, fmt.Errorf("write /etc/iptables/rules.v4: %w", err)
	}

	// Install the systemd restore unit.
	const svcPath = "/etc/systemd/system/iptables-restore.service"
	if err := fsys.WriteFile(svcPath, iptablesRestoreService, 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", svcPath, err)
	}

	// Reload systemd and enable the unit.
	if _, err := runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return Result{}, fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if _, err := runner.Run(ctx, "systemctl", "enable", "iptables-restore"); err != nil {
		return Result{}, fmt.Errorf("systemctl enable iptables-restore: %w", err)
	}

	return Result{
		Applied: true,
		Extra: map[string]interface{}{
			"distro":     "flatcar",
			"allow_icmp": h.AllowICMP,
			"ssh_from":   TailscaleCGNAT,
		},
	}, nil
}

// Remove tears down the firewall configuration.
//
// On Ubuntu (nil distro or "ubuntu"): no-op for v1 — disabling the firewall
// on a hardened node would be the wrong default; T4b will revisit this.
//
// On Flatcar: flushes iptables, removes the rules file, and disables/removes
// the systemd unit.
func (s *Section) Remove(ctx context.Context, spec *config.Spec) (Result, error) {
	if s.Distro != nil && s.Distro.ID() == "flatcar" {
		return s.removeFlatcar(ctx)
	}
	return Result{Applied: false, Reason: "remove not implemented"}, nil
}

// removeFlatcar tears down the Flatcar iptables configuration.
func (s *Section) removeFlatcar(ctx context.Context) (Result, error) {
	runner := s.runner()
	fsys := s.fsys()

	// Flush all rules.
	if _, err := runner.Run(ctx, "iptables", "-F"); err != nil {
		return Result{}, fmt.Errorf("iptables -F: %w", err)
	}

	// Remove the persisted rules file (best-effort — ignore not-found).
	const rulesPath = "/etc/iptables/rules.v4"
	if err := fsys.WriteFile(rulesPath, nil, 0); err != nil {
		// WriteFile with nil content signals removal; ignore if already gone.
		_ = err
	}

	// Disable and remove the systemd unit.
	const svcPath = "/etc/systemd/system/iptables-restore.service"
	if _, err := runner.Run(ctx, "systemctl", "disable", "iptables-restore"); err != nil {
		return Result{}, fmt.Errorf("systemctl disable iptables-restore: %w", err)
	}
	if err := fsys.WriteFile(svcPath, nil, 0); err != nil {
		_ = err
	}

	return Result{Applied: true, Reason: "flatcar iptables flushed"}, nil
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

func (osFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	if data == nil {
		return os.Remove(path)
	}
	return os.WriteFile(path, data, perm)
}
