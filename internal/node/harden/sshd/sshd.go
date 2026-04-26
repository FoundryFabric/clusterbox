// Package sshd implements the sshd-hardening subsystem of the harden
// section.
//
// It writes a drop-in /etc/ssh/sshd_config.d/99-clusterbox.conf with
// PermitRootLogin no, PasswordAuthentication no,
// KbdInteractiveAuthentication no, and PubkeyAuthentication yes,
// validates the resulting config with `sshd -t`, and reloads the ssh
// unit via systemctl. Idempotent: if the on-disk config already matches
// the embedded payload byte-for-byte the reload is skipped.
package sshd

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

// DropInPath is the absolute path of the drop-in we manage. Exposed as
// a var (not const) so tests can redirect it into a temp dir.
var DropInPath = "/etc/ssh/sshd_config.d/99-clusterbox.conf"

//go:embed conf/99-clusterbox.conf
var dropInPayload []byte

// Runner abstracts process execution so unit tests can inject a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// FS abstracts filesystem reads/writes so tests do not require root
// access to /etc/ssh.
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

// Apply writes the drop-in, validates the merged sshd config, and
// reloads ssh.
//
// Behaviour matrix:
//
//   - spec.Harden nil or Enabled=false: Applied=false, Reason="disabled".
//   - drop-in already matches embedded payload: skip write+reload, but
//     still report applied=true so the harden coordinator can flag the
//     subsystem as healthy.
//   - sshd -t fails: write is rolled back to the previous content (or
//     removed if there wasn't one) and the validation error is returned.
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	if h := specHarden(spec); h == nil || !h.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}

	runner, fsys := s.runner(), s.fsys()

	prev, prevExists, err := readPrev(fsys, DropInPath)
	if err != nil {
		return Result{}, err
	}

	if prevExists && bytes.Equal(prev, dropInPayload) {
		return Result{
			Applied: true,
			Reason:  "already configured",
			Extra: map[string]interface{}{
				"drop_in":     DropInPath,
				"reloaded":    false,
				"already_set": true,
			},
		}, nil
	}

	// Ensure /etc/ssh/sshd_config.d/ exists. On stock Debian/Ubuntu it
	// already does, but this keeps the subsystem usable on more minimal
	// images.
	if err := fsys.MkdirAll("/etc/ssh/sshd_config.d", 0o755); err != nil {
		return Result{}, fmt.Errorf("sshd: mkdir sshd_config.d: %w", err)
	}
	if err := fsys.WriteFile(DropInPath, dropInPayload, 0o644); err != nil {
		return Result{}, fmt.Errorf("sshd: write %s: %w", DropInPath, err)
	}

	if _, err := runner.Run(ctx, "sshd", "-t"); err != nil {
		// Roll back so a broken config does not survive the failed Apply.
		if prevExists {
			_ = fsys.WriteFile(DropInPath, prev, 0o644)
		} else {
			_ = removeFile(fsys, DropInPath)
		}
		return Result{}, fmt.Errorf("sshd: validate: %w", err)
	}

	if _, err := runner.Run(ctx, "systemctl", "reload", "ssh"); err != nil {
		return Result{}, fmt.Errorf("sshd: reload: %w", err)
	}

	return Result{
		Applied: true,
		Extra: map[string]interface{}{
			"drop_in":     DropInPath,
			"reloaded":    true,
			"already_set": false,
		},
	}, nil
}

// Remove is a no-op for v1.
//
// Tearing down the drop-in mid-flight would re-open password auth on a
// node still hosting workloads; T4b will revisit this once uninstall
// semantics are fully spec'd.
func (s *Section) Remove(_ context.Context, _ *config.Spec) (Result, error) {
	return Result{Applied: false, Reason: "remove not implemented"}, nil
}

// readPrev reads the existing drop-in if any, returning (data, true, nil)
// when present and (nil, false, nil) when absent. Other errors are
// surfaced.
func readPrev(fsys FS, path string) ([]byte, bool, error) {
	data, err := fsys.ReadFile(path)
	if err == nil {
		return data, true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("sshd: read %s: %w", path, err)
}

// removeFile is implemented in terms of WriteFile so tests with the
// in-memory FS still observe the rollback.
//
// On the production osFS the call falls through to os.Remove (see the
// concrete implementation below); on the fake it deletes the entry from
// the in-memory map. We expose this as a small interface assertion so
// the interface contract for FS does not have to change.
func removeFile(fsys FS, path string) error {
	if r, ok := fsys.(interface{ Remove(string) error }); ok {
		return r.Remove(path)
	}
	// Fallback: best-effort overwrite with empty bytes. Callers only
	// invoke this on the rollback path so a missing Remove method is
	// not catastrophic.
	return fsys.WriteFile(path, nil, 0o644)
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

// osFS is the production [FS] backed by the real filesystem.
type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error)        { return os.Stat(path) }
func (osFS) ReadFile(path string) ([]byte, error)         { return os.ReadFile(path) }
func (osFS) MkdirAll(path string, mode fs.FileMode) error { return os.MkdirAll(path, mode) }
func (osFS) WriteFile(path string, data []byte, mode fs.FileMode) error {
	return os.WriteFile(path, data, mode)
}
func (osFS) Remove(path string) error { return os.Remove(path) }
