// Package fail2ban implements the fail2ban subsystem of the harden section.
//
// Apply installs fail2ban if missing, drops the jail.d/clusterbox.conf
// config (sshd jail, systemd backend, 3600s bantime), and enables + starts
// the service. Idempotent: if the service is already enabled the config is
// still written (in case it drifted), but the enable/start calls are safe
// to repeat via systemctl --no-block.
package fail2ban

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

// JailPath is the absolute path of the jail.d drop-in we manage.
var JailPath = "/etc/fail2ban/jail.d/clusterbox.conf"

//go:embed conf/clusterbox.conf
var jailPayload []byte

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

// Apply installs fail2ban, writes the jail config, and enables + starts the service.
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

	if err := s.writeJailConfig(fsys); err != nil {
		return Result{}, err
	}

	if _, err := runner.Run(ctx, "systemctl", "enable", "fail2ban"); err != nil {
		return Result{}, fmt.Errorf("fail2ban: systemctl enable: %w", err)
	}
	if _, err := runner.Run(ctx, "systemctl", "start", "fail2ban"); err != nil {
		return Result{}, fmt.Errorf("fail2ban: systemctl start: %w", err)
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
	if _, err := fsys.Stat("/usr/bin/fail2ban-server"); err == nil {
		return false, nil
	}
	env := []string{"DEBIAN_FRONTEND=noninteractive"}
	if _, err := runner.RunEnv(ctx, env, "apt-get", "update", "-qq"); err != nil {
		return false, fmt.Errorf("fail2ban: apt-get update: %w", err)
	}
	if _, err := runner.RunEnv(ctx, env, "apt-get", "install", "-y", "-qq", "fail2ban"); err != nil {
		return false, fmt.Errorf("fail2ban: apt-get install: %w", err)
	}
	return true, nil
}

func (s *Section) writeJailConfig(fsys FS) error {
	existing, err := fsys.ReadFile(JailPath)
	if err == nil && bytes.Equal(existing, jailPayload) {
		return nil
	}
	if err := fsys.MkdirAll("/etc/fail2ban/jail.d", 0o755); err != nil {
		return fmt.Errorf("fail2ban: mkdir jail.d: %w", err)
	}
	if err := fsys.WriteFile(JailPath, jailPayload, 0o644); err != nil {
		return fmt.Errorf("fail2ban: write jail config: %w", err)
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
