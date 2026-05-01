// Package auditd implements the auditd subsystem of the harden section.
//
// Apply installs auditd if missing, drops the audit rules file at
// /etc/audit/rules.d/clusterbox.rules (covering logins, privilege
// escalation, SSH config changes, identity files, and kernel module
// loading), and enables + starts the service. Idempotent.
package auditd

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/distro"
)

// RulesPath is the absolute path of the audit rules file we manage.
var RulesPath = "/etc/audit/rules.d/clusterbox.rules"

//go:embed conf/clusterbox.rules
var rulesPayload []byte

// Runner abstracts process execution so unit tests can inject a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
}

// FS abstracts filesystem reads/writes.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, mode fs.FileMode) error
	MkdirAll(path string, mode fs.FileMode) error
}

// Result is the structured payload returned by Apply / Remove.
type Result struct {
	Applied    bool
	Reason     string
	Skipped    bool
	SkipReason string
	Extra      map[string]interface{}
}

// Section bundles the dependencies used by Apply and Remove.
type Section struct {
	Runner Runner
	FS     FS
	Distro distro.Distro
}

// Apply installs auditd, writes the rules file, and enables + starts the service.
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	if s.Distro != nil && s.Distro.ID() != "ubuntu" {
		return Result{Skipped: true, SkipReason: "not supported on " + s.Distro.ID()}, nil
	}

	h := specHarden(spec)
	if h == nil || !h.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}

	runner, fsys := s.runner(), s.fsys()

	installed, err := s.ensureInstalled(ctx, runner, fsys)
	if err != nil {
		return Result{}, err
	}

	if err := s.writeRules(fsys); err != nil {
		return Result{}, err
	}

	if _, err := runner.Run(ctx, "systemctl", "enable", "auditd"); err != nil {
		return Result{}, fmt.Errorf("auditd: systemctl enable: %w", err)
	}
	if _, err := runner.Run(ctx, "systemctl", "start", "auditd"); err != nil {
		return Result{}, fmt.Errorf("auditd: systemctl start: %w", err)
	}

	return Result{
		Applied: true,
		Extra:   map[string]interface{}{"installed": installed},
	}, nil
}

// Remove is a no-op for v1.
func (s *Section) Remove(_ context.Context, _ *config.Spec) (Result, error) {
	return Result{Applied: false, Reason: "remove not implemented"}, nil
}

func (s *Section) ensureInstalled(ctx context.Context, runner Runner, fsys FS) (bool, error) {
	if _, err := fsys.Stat("/usr/sbin/auditd"); err == nil {
		return false, nil
	}
	env := []string{"DEBIAN_FRONTEND=noninteractive"}
	if _, err := runner.RunEnv(ctx, env, "apt-get", "update", "-qq"); err != nil {
		return false, fmt.Errorf("auditd: apt-get update: %w", err)
	}
	if _, err := runner.RunEnv(ctx, env, "apt-get", "install", "-y", "-qq", "auditd", "audispd-plugins"); err != nil {
		return false, fmt.Errorf("auditd: apt-get install: %w", err)
	}
	return true, nil
}

func (s *Section) writeRules(fsys FS) error {
	existing, err := fsys.ReadFile(RulesPath)
	if err == nil && bytes.Equal(existing, rulesPayload) {
		return nil
	}
	if err := fsys.MkdirAll("/etc/audit/rules.d", 0o755); err != nil {
		return fmt.Errorf("auditd: mkdir rules.d: %w", err)
	}
	if err := fsys.WriteFile(RulesPath, rulesPayload, 0o640); err != nil {
		return fmt.Errorf("auditd: write rules: %w", err)
	}
	return nil
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

type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error)                { return os.Stat(path) }
func (osFS) ReadFile(path string) ([]byte, error)                 { return os.ReadFile(path) }
func (osFS) WriteFile(path string, d []byte, m fs.FileMode) error { return os.WriteFile(path, d, m) }
func (osFS) MkdirAll(path string, m fs.FileMode) error            { return os.MkdirAll(path, m) }
