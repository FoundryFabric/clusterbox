// Package unattended implements the unattended-upgrades subsystem of the
// harden section.
//
// Apply installs unattended-upgrades if missing, writes the two apt config
// files (50unattended-upgrades for security-channel-only policy, 20auto-upgrades
// for daily scheduling), and enables the service. Idempotent: each file is
// only written when the on-disk content differs from the embedded payload.
package unattended

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

// UpgradesPath is the absolute path of the security-policy config file.
var UpgradesPath = "/etc/apt/apt.conf.d/50unattended-upgrades"

// AutoPath is the absolute path of the periodic-scheduling config file.
var AutoPath = "/etc/apt/apt.conf.d/20auto-upgrades"

//go:embed conf/50unattended-upgrades
var upgradesPayload []byte

//go:embed conf/20auto-upgrades
var autoPayload []byte

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
	Applied bool
	Reason  string
	Extra   map[string]interface{}
}

// Section bundles the dependencies used by Apply and Remove.
type Section struct {
	Runner Runner
	FS     FS
}

// Apply installs unattended-upgrades, writes the policy files, and enables the service.
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

	if err := writeIfChanged(fsys, UpgradesPath, upgradesPayload); err != nil {
		return Result{}, fmt.Errorf("unattended: write 50unattended-upgrades: %w", err)
	}
	if err := writeIfChanged(fsys, AutoPath, autoPayload); err != nil {
		return Result{}, fmt.Errorf("unattended: write 20auto-upgrades: %w", err)
	}

	if _, err := runner.Run(ctx, "systemctl", "enable", "unattended-upgrades"); err != nil {
		return Result{}, fmt.Errorf("unattended: systemctl enable: %w", err)
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
	if _, err := fsys.Stat("/usr/bin/unattended-upgrade"); err == nil {
		return false, nil
	}
	env := []string{"DEBIAN_FRONTEND=noninteractive"}
	if _, err := runner.RunEnv(ctx, env, "apt-get", "update", "-qq"); err != nil {
		return false, fmt.Errorf("unattended: apt-get update: %w", err)
	}
	if _, err := runner.RunEnv(ctx, env, "apt-get", "install", "-y", "-qq", "unattended-upgrades", "apt-listchanges"); err != nil {
		return false, fmt.Errorf("unattended: apt-get install: %w", err)
	}
	return true, nil
}

func writeIfChanged(fsys FS, path string, payload []byte) error {
	existing, err := fsys.ReadFile(path)
	if err == nil && bytes.Equal(existing, payload) {
		return nil
	}
	if err := fsys.MkdirAll("/etc/apt/apt.conf.d", 0o755); err != nil {
		return fmt.Errorf("mkdir apt.conf.d: %w", err)
	}
	return fsys.WriteFile(path, payload, 0o644)
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
