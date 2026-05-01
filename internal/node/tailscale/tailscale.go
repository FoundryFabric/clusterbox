// Package tailscale implements the Tailscale enrolment section for
// clusterboxnode. It writes the embedded binaries (when present), installs
// tailscaled.service, runs tailscale up with the configured auth key, and
// polls until the node is running.
package tailscale

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

//go:embed assets/tailscaled.service
var tailscaledService []byte

const (
	tailscaleBin   = "/usr/local/bin/tailscale"
	tailscaledBin  = "/usr/local/bin/tailscaled"
	tailscaledUnit = "/etc/systemd/system/tailscaled.service"
	pollInterval   = 2 * time.Second
	pollTimeout    = 30 * time.Second
)

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
	// nowFunc is used in tests to control time; nil means time.Now.
	nowFunc func() time.Time
	// afterFunc is used in tests to control sleep; nil means time.Sleep.
	afterFunc func(time.Duration)
}

// Apply installs and starts Tailscale on the node according to spec.
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	if spec.Tailscale == nil || !spec.Tailscale.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}
	ts := spec.Tailscale
	runner, fsys := s.runner(), s.fsys()

	// Idempotency check: already enrolled and running.
	alreadyRunning, _, err := isRunning(ctx, runner)
	if err == nil && alreadyRunning {
		return Result{Applied: false, Reason: "already running"}, nil
	}

	// Write embedded binaries if present.
	if len(EmbeddedTailscaled) > 0 {
		if err := fsys.WriteFile(tailscaledBin, EmbeddedTailscaled, 0o755); err != nil {
			return Result{}, fmt.Errorf("tailscale: write tailscaled: %w", err)
		}
	}
	if len(EmbeddedTailscale) > 0 {
		if err := fsys.WriteFile(tailscaleBin, EmbeddedTailscale, 0o755); err != nil {
			return Result{}, fmt.Errorf("tailscale: write tailscale: %w", err)
		}
	}

	// Write systemd unit.
	if err := fsys.WriteFile(tailscaledUnit, tailscaledService, 0o644); err != nil {
		return Result{}, fmt.Errorf("tailscale: write tailscaled.service: %w", err)
	}

	if _, err := runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return Result{}, fmt.Errorf("tailscale: daemon-reload: %w", err)
	}
	if _, err := runner.Run(ctx, "systemctl", "enable", "tailscaled"); err != nil {
		return Result{}, fmt.Errorf("tailscale: systemctl enable tailscaled: %w", err)
	}
	if _, err := runner.Run(ctx, "systemctl", "start", "tailscaled"); err != nil {
		return Result{}, fmt.Errorf("tailscale: systemctl start tailscaled: %w", err)
	}

	// Build tailscale up args.
	upArgs := []string{"up", "--authkey=" + ts.AuthKey}
	if ts.AcceptRoutes {
		upArgs = append(upArgs, "--accept-routes")
	}
	if ts.AcceptDNS {
		upArgs = append(upArgs, "--accept-dns")
	}
	if ts.Hostname != "" {
		upArgs = append(upArgs, "--hostname="+ts.Hostname)
	}
	if _, err := runner.Run(ctx, "tailscale", upArgs...); err != nil {
		return Result{}, fmt.Errorf("tailscale: tailscale up: %w", err)
	}

	// Poll until running.
	ip, err := s.pollUntilRunning(ctx, runner)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Applied: true,
		Extra:   map[string]interface{}{"ip": ip},
	}, nil
}

// Remove tears down Tailscale: stops and disables tailscaled, removes the
// service file and binaries.
func (s *Section) Remove(ctx context.Context, _ *config.Spec) (Result, error) {
	fsys := s.fsys()
	runner := s.runner()

	// Check if tailscaled binary is present.
	if _, err := fsys.Stat(tailscaledBin); err != nil {
		return Result{Applied: false, Reason: "not installed"}, nil
	}

	if _, err := runner.Run(ctx, "systemctl", "stop", "tailscaled"); err != nil {
		return Result{}, fmt.Errorf("tailscale: systemctl stop tailscaled: %w", err)
	}
	if _, err := runner.Run(ctx, "systemctl", "disable", "tailscaled"); err != nil {
		return Result{}, fmt.Errorf("tailscale: systemctl disable tailscaled: %w", err)
	}

	// Remove unit file (best-effort: ignore not-found).
	_ = removeFile(fsys, tailscaledUnit)
	_ = removeFile(fsys, tailscaledBin)
	_ = removeFile(fsys, tailscaleBin)

	return Result{Applied: true}, nil
}

// removeFile deletes path via the optional Remove extension on FS. The core FS
// interface does not expose Remove (following the fail2ban pattern), so osFS
// extends it with Remove backed by os.Remove. Any FS that does not implement
// the extension (e.g. a minimal test fake) gets a silent no-op — callers
// already ignore the return value for best-effort cleanup.
func removeFile(fsys FS, path string) error {
	if o, ok := fsys.(interface{ Remove(string) error }); ok {
		return o.Remove(path)
	}
	return nil
}

// tailscaleStatus is a minimal subset of `tailscale status --json`.
type tailscaleStatus struct {
	BackendState string `json:"BackendState"`
	Self         struct {
		TailscaleIPs []string `json:"TailscaleIPs"`
	} `json:"Self"`
}

// isRunning queries tailscale status and returns whether the node is enrolled
// and running, along with the first IP if running.
func isRunning(ctx context.Context, runner Runner) (bool, string, error) {
	out, err := runner.Run(ctx, "tailscale", "status", "--json")
	if err != nil {
		return false, "", err
	}
	var st tailscaleStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return false, "", fmt.Errorf("tailscale: parse status: %w", err)
	}
	if st.BackendState == "Running" && len(st.Self.TailscaleIPs) > 0 {
		return true, st.Self.TailscaleIPs[0], nil
	}
	return false, "", nil
}

// pollUntilRunning polls tailscale status until BackendState=="Running" and
// TailscaleIPs is non-empty, or until pollTimeout elapses.
func (s *Section) pollUntilRunning(ctx context.Context, runner Runner) (string, error) {
	now := s.nowFunc
	if now == nil {
		now = time.Now
	}
	sleep := s.afterFunc
	if sleep == nil {
		sleep = time.Sleep
	}

	deadline := now().Add(pollTimeout)
	for {
		running, ip, err := isRunning(ctx, runner)
		if err == nil && running {
			return ip, nil
		}
		if now().After(deadline) {
			return "", fmt.Errorf("tailscale: timed out waiting for Running state after %s", pollTimeout)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("tailscale: context cancelled while waiting for Running state: %w", ctx.Err())
		default:
		}
		sleep(pollInterval)
	}
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

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, string(out))
	}
	return out, nil
}

func (execRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, string(out))
	}
	return out, nil
}

type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error)                { return os.Stat(path) }
func (osFS) ReadFile(path string) ([]byte, error)                 { return os.ReadFile(path) }
func (osFS) WriteFile(path string, d []byte, m fs.FileMode) error { return os.WriteFile(path, d, m) }
func (osFS) MkdirAll(path string, m fs.FileMode) error            { return os.MkdirAll(path, m) }
func (osFS) Remove(path string) error                             { return os.Remove(path) }
