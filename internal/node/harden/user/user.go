// Package user implements the user-creation subsystem of the harden
// section.
//
// It creates the non-root operator account named in the spec (defaulting
// to "clusterbox"), adds it to the sudo group, and installs the supplied
// SSH public key in ~/.ssh/authorized_keys with mode 0600. Operations
// are idempotent: existing users and existing key entries are detected
// rather than recreated.
package user

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

// DefaultUser is the operator account name used when the spec leaves
// Harden.User empty.
const DefaultUser = "clusterbox"

// Runner abstracts process execution so unit tests can inject a fake.
//
// Only Run is needed for the user subsystem — there is no shell
// pipeline. Runner mirrors the shape used elsewhere in node/* to keep
// fakes interchangeable across subsystems.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// FS abstracts filesystem reads/writes/ownership so tests do not need
// privileged access to /home.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	MkdirAll(path string, mode fs.FileMode) error
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, mode fs.FileMode) error
	Chown(path string, uid, gid int) error
	Chmod(path string, mode fs.FileMode) error
	LookupUserHome(name string) (homeDir string, uid, gid int, err error)
}

// Result is the structured payload returned by Apply / Remove.
//
// The harden coordinator translates this into the per-section JSON
// shape expected by install.SectionResult.
type Result struct {
	Applied bool
	Reason  string
	Extra   map[string]interface{}
}

// Section bundles the dependencies used by Apply and Remove. The zero
// value uses the real os/exec runner and a real-filesystem FS.
type Section struct {
	Runner Runner
	FS     FS
}

// Apply creates the user account if missing, adds it to sudo, and
// installs the SSH key.
//
// Behaviour matrix:
//
//   - spec.Harden nil or Enabled=false: Applied=false, Reason="disabled".
//   - user already exists: skip useradd; still ensure sudo membership and
//     authorized_keys.
//   - SSH key already present in authorized_keys: skip append.
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	h := specHarden(spec)
	if h == nil || !h.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}
	username := h.User
	if username == "" {
		username = DefaultUser
	}
	if h.SSHPubKey == "" {
		return Result{}, errors.New("user: ssh_pub_key is required")
	}

	runner, fsys := s.runner(), s.fsys()

	created, err := ensureUser(ctx, runner, fsys, username)
	if err != nil {
		return Result{}, err
	}
	if err := ensureSudoGroup(ctx, runner, username); err != nil {
		return Result{}, err
	}
	keyAdded, err := ensureAuthorizedKey(fsys, username, h.SSHPubKey)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Applied: true,
		Extra: map[string]interface{}{
			"user":         username,
			"user_created": created,
			"key_added":    keyAdded,
		},
	}, nil
}

// Remove is a no-op for v1.
//
// Removing the operator user mid-flight on a live node is far more
// dangerous than leaving it; T4b is expected to revisit this once
// uninstall semantics are properly defined.
func (s *Section) Remove(_ context.Context, _ *config.Spec) (Result, error) {
	return Result{Applied: false, Reason: "remove not implemented"}, nil
}

// ensureUser creates the user via `useradd` if it does not yet exist.
// Returns true when a creation actually happened.
func ensureUser(ctx context.Context, runner Runner, fsys FS, username string) (bool, error) {
	if _, _, _, err := fsys.LookupUserHome(username); err == nil {
		return false, nil
	}
	if _, err := runner.Run(ctx, "useradd", "--create-home", "--shell", "/bin/bash", username); err != nil {
		return false, fmt.Errorf("user: useradd %s: %w", username, err)
	}
	return true, nil
}

// ensureSudoGroup is idempotent: usermod -aG only adds memberships that
// are missing, and the command itself is safe to repeat.
func ensureSudoGroup(ctx context.Context, runner Runner, username string) error {
	if _, err := runner.Run(ctx, "usermod", "-aG", "sudo", username); err != nil {
		return fmt.Errorf("user: usermod -aG sudo %s: %w", username, err)
	}
	return nil
}

// ensureAuthorizedKey writes the key into ~/.ssh/authorized_keys if it
// is not already present. Returns true if the file was modified.
func ensureAuthorizedKey(fsys FS, username, pubKey string) (bool, error) {
	home, uid, gid, err := fsys.LookupUserHome(username)
	if err != nil {
		return false, fmt.Errorf("user: lookup home for %s: %w", username, err)
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := fsys.MkdirAll(sshDir, 0o700); err != nil {
		return false, fmt.Errorf("user: mkdir %s: %w", sshDir, err)
	}
	if err := fsys.Chmod(sshDir, 0o700); err != nil {
		return false, fmt.Errorf("user: chmod %s: %w", sshDir, err)
	}
	if err := fsys.Chown(sshDir, uid, gid); err != nil {
		return false, fmt.Errorf("user: chown %s: %w", sshDir, err)
	}

	keyPath := filepath.Join(sshDir, "authorized_keys")
	existing, err := fsys.ReadFile(keyPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("user: read %s: %w", keyPath, err)
	}

	want := strings.TrimRight(pubKey, "\n") + "\n"
	if keyAlreadyPresent(existing, pubKey) {
		// Still enforce mode/ownership in case a prior run left them wrong.
		if err := fsys.Chmod(keyPath, 0o600); err != nil {
			return false, fmt.Errorf("user: chmod %s: %w", keyPath, err)
		}
		if err := fsys.Chown(keyPath, uid, gid); err != nil {
			return false, fmt.Errorf("user: chown %s: %w", keyPath, err)
		}
		return false, nil
	}

	merged := append(append([]byte{}, existing...), want...)
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		merged = append(append(append([]byte{}, existing...), '\n'), want...)
	}
	if err := fsys.WriteFile(keyPath, merged, 0o600); err != nil {
		return false, fmt.Errorf("user: write %s: %w", keyPath, err)
	}
	if err := fsys.Chown(keyPath, uid, gid); err != nil {
		return false, fmt.Errorf("user: chown %s: %w", keyPath, err)
	}
	return true, nil
}

func keyAlreadyPresent(haystack []byte, key string) bool {
	wantTrim := strings.TrimSpace(key)
	for _, line := range strings.Split(string(haystack), "\n") {
		if strings.TrimSpace(line) == wantTrim {
			return true
		}
	}
	return false
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

// osFS is the production [FS] backed by the real filesystem and the
// system password database.
type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error)        { return os.Stat(path) }
func (osFS) MkdirAll(path string, mode fs.FileMode) error { return os.MkdirAll(path, mode) }
func (osFS) ReadFile(path string) ([]byte, error)         { return os.ReadFile(path) }
func (osFS) Chown(path string, uid, gid int) error        { return os.Chown(path, uid, gid) }
func (osFS) Chmod(path string, mode fs.FileMode) error    { return os.Chmod(path, mode) }
func (osFS) WriteFile(p string, d []byte, m fs.FileMode) error {
	return os.WriteFile(p, d, m)
}

// LookupUserHome resolves a username to its home directory and numeric
// uid/gid via the standard library os/user package.
func (osFS) LookupUserHome(name string) (string, int, int, error) {
	return lookupUserHome(name)
}
