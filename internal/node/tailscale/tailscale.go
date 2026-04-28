// Package tailscale implements the Tailscale install/uninstall section of
// clusterboxnode.
//
// The install path:
//
//   - spec.Tailscale nil or Enabled=false: Applied=false, Reason="disabled".
//   - tailscale already present and connected: Applied=true, Reason="already installed".
//   - otherwise: run the official install script, then tailscale up.
//
// Auth key resolution: AuthKey is used directly; AuthKeyEnv names an
// environment variable that must already be set in the process env (the
// provider passes it via envOverlay on the remote install command).
package tailscale

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

// Package-level vars so tests can override without touching real paths.
var (
	// TailscaleBinary is the expected location of the tailscale CLI.
	TailscaleBinary = "/usr/bin/tailscale"

	// InstallURL is the official Tailscale install script endpoint.
	InstallURL = "https://tailscale.com/install.sh"
)

// Runner abstracts process execution so unit tests can inject a fake.
type Runner interface {
	// Run executes name with args and returns combined output.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// RunShell executes a shell pipeline under /bin/sh -c.
	RunShell(ctx context.Context, env []string, script string) ([]byte, error)
}

// FS abstracts filesystem stat so tests do not need real paths.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
}

// Result is the structured payload returned by Apply / Remove.
type Result struct {
	Applied bool
	Reason  string
	Extra   map[string]any
}

// Section bundles the dependencies used by Apply and Remove.
// All fields have working zero-value defaults for production use.
type Section struct {
	Runner Runner
	FS     FS
}

// Apply installs and brings up Tailscale when enabled.
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	t := specTailscale(spec)
	if t == nil || !t.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}

	authKey, err := resolveAuthKey(t)
	if err != nil {
		return Result{}, err
	}

	runner, fsys := s.runner(), s.fsys()

	already, err := s.alreadyInstalled(fsys)
	if err != nil {
		return Result{}, err
	}
	if !already {
		script := fmt.Sprintf("curl -fsSL %s | sh", InstallURL)
		if _, err := runner.RunShell(ctx, nil, script); err != nil {
			return Result{}, fmt.Errorf("tailscale: install script: %w", err)
		}
	}

	upArgs := []string{"up", "--authkey=" + authKey, "--accept-routes", "--accept-dns"}
	if t.Hostname != "" {
		upArgs = append(upArgs, "--hostname="+t.Hostname)
	}
	if _, err := runner.Run(ctx, TailscaleBinary, upArgs...); err != nil {
		return Result{}, fmt.Errorf("tailscale up: %w", err)
	}

	// Read the assigned IP for the result Extra payload.
	ipOut, _ := runner.Run(ctx, TailscaleBinary, "ip", "--1")
	tailscaleIP := strings.TrimSpace(string(ipOut))

	res := Result{
		Applied: true,
		Extra: map[string]any{
			"tailscale_ip": tailscaleIP,
		},
	}
	if already {
		res.Reason = "already installed"
	}
	if t.Hostname != "" {
		res.Extra["hostname"] = t.Hostname
	}
	return res, nil
}

// Remove disconnects Tailscale and (best-effort) uninstalls the daemon.
func (s *Section) Remove(ctx context.Context, _ *config.Spec) (Result, error) {
	runner, fsys := s.runner(), s.fsys()

	present, err := s.alreadyInstalled(fsys)
	if err != nil {
		return Result{}, err
	}
	if !present {
		return Result{
			Applied: false,
			Reason:  "tailscale not installed",
			Extra: map[string]any{
				"removed":              false,
				"tailscale_was_present": false,
			},
		}, nil
	}

	if _, err := runner.Run(ctx, TailscaleBinary, "logout"); err != nil {
		return Result{}, fmt.Errorf("tailscale logout: %w", err)
	}
	return Result{
		Applied: true,
		Extra: map[string]any{
			"removed":              true,
			"tailscale_was_present": true,
		},
	}, nil
}

func (s *Section) alreadyInstalled(fsys FS) (bool, error) {
	_, err := fsys.Stat(TailscaleBinary)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("tailscale: stat %s: %w", TailscaleBinary, err)
}

func resolveAuthKey(t *config.TailscaleSpec) (string, error) {
	if t.AuthKey != "" {
		return t.AuthKey, nil
	}
	if t.AuthKeyEnv != "" {
		v := os.Getenv(t.AuthKeyEnv)
		if v == "" {
			return "", fmt.Errorf("tailscale: auth key env %q is not set", t.AuthKeyEnv)
		}
		return v, nil
	}
	return "", fmt.Errorf("tailscale: no auth key configured")
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

func specTailscale(spec *config.Spec) *config.TailscaleSpec {
	if spec == nil {
		return nil
	}
	return spec.Tailscale
}

// execRunner is the production Runner backed by os/exec.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %v: %w: %s", name, args, err, string(out))
	}
	return out, nil
}

func (execRunner) RunShell(ctx context.Context, env []string, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("/bin/sh -c %q: %w: %s", script, err, string(out))
	}
	return out, nil
}

// osFS is the production FS backed by the real filesystem.
type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
