// Package sysctl implements the sysctl-hardening subsystem of the harden
// section.
//
// Apply writes /etc/sysctl.d/99-clusterbox.conf (IP spoofing protection,
// redirect suppression, SYN-cookie flood mitigation, dmesg/core-dump/kptr
// restrictions) and runs `sysctl --system` to apply the settings live.
// Idempotent: the file is only written when the on-disk content differs
// from the embedded payload; sysctl --system is always run to ensure the
// live kernel reflects the config.
package sysctl

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

// ConfPath is the absolute path of the sysctl drop-in we manage.
var ConfPath = "/etc/sysctl.d/99-clusterbox.conf"

//go:embed conf/99-clusterbox.conf
var confPayload []byte

// Runner abstracts process execution so unit tests can inject a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
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

// Apply writes the sysctl drop-in and activates the settings.
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	h := specHarden(spec)
	if h == nil || !h.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}

	runner, fsys := s.runner(), s.fsys()

	wrote, err := s.writeConf(fsys)
	if err != nil {
		return Result{}, err
	}

	// Always run sysctl --system so the live kernel reflects the config,
	// even when the file was already up to date.
	if _, err := runner.Run(ctx, "sysctl", "--system"); err != nil {
		return Result{}, fmt.Errorf("sysctl: --system: %w", err)
	}

	return Result{
		Applied: true,
		Extra:   map[string]interface{}{"conf_written": wrote},
	}, nil
}

// Remove is a no-op for v1.
func (s *Section) Remove(_ context.Context, _ *config.Spec) (Result, error) {
	return Result{Applied: false, Reason: "remove not implemented"}, nil
}

// writeConf writes the embedded payload when the on-disk content differs.
// Returns true when a write actually occurred.
func (s *Section) writeConf(fsys FS) (bool, error) {
	existing, err := fsys.ReadFile(ConfPath)
	if err == nil && bytes.Equal(existing, confPayload) {
		return false, nil
	}
	if err := fsys.MkdirAll("/etc/sysctl.d", 0o755); err != nil {
		return false, fmt.Errorf("sysctl: mkdir sysctl.d: %w", err)
	}
	if err := fsys.WriteFile(ConfPath, confPayload, 0o644); err != nil {
		return false, fmt.Errorf("sysctl: write conf: %w", err)
	}
	return true, nil
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

type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error)                { return os.Stat(path) }
func (osFS) ReadFile(path string) ([]byte, error)                 { return os.ReadFile(path) }
func (osFS) WriteFile(path string, d []byte, m fs.FileMode) error { return os.WriteFile(path, d, m) }
func (osFS) MkdirAll(path string, m fs.FileMode) error            { return os.MkdirAll(path, m) }
