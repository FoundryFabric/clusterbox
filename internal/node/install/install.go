// Package install implements the section walker shared by the
// `clusterboxnode install` and `clusterboxnode uninstall` subcommands.
//
// The walker iterates a fixed, ordered list of Sections (harden, tailscale,
// k3s) and accumulates a per-section result map. For install, any section
// returning an error stops the walk and an error-shape JSON document is
// emitted. For uninstall, errors are recorded onto the per-section result and
// the walk continues so as much state as possible is torn down.
package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/distro"
	"github.com/foundryfabric/clusterbox/internal/node/harden"
	"github.com/foundryfabric/clusterbox/internal/node/k3s"
	"github.com/foundryfabric/clusterbox/internal/node/tailscale"
)

// SectionResult captures the structured output of a single section.
//
// "applied" is the only required field; sections add additional keys
// (e.g. "version", "steps") via WithExtra. Stub sections set Applied=false
// and Reason="section not implemented yet".
//
// When a section is skipped (e.g. apt-only subsystem on a non-Ubuntu distro),
// Skipped is true and SkipReason explains why. Skipped is not an error.
type SectionResult struct {
	Applied    bool           `json:"applied"`
	Reason     string         `json:"reason,omitempty"`
	Skipped    bool           `json:"-"`
	SkipReason string         `json:"-"`
	Error      string         `json:"error,omitempty"`
	Extra      map[string]any `json:"-"`
}

// MarshalJSON flattens Extra into the top-level object so that section-
// specific keys appear alongside the standard fields.
func (r SectionResult) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	maps.Copy(out, r.Extra)
	out["applied"] = r.Applied
	if r.Reason != "" {
		out["reason"] = r.Reason
	}
	if r.Skipped {
		out["skipped"] = true
		if r.SkipReason != "" {
			out["skip_reason"] = r.SkipReason
		}
	}
	if r.Error != "" {
		out["error"] = r.Error
	}
	return json.Marshal(out)
}

// Section is a single install/uninstall step.
//
// Each section returns its own SectionResult. Returning a non-nil error from
// Run signals a hard failure: install stops the walk; uninstall records the
// error onto the result and continues.
type Section interface {
	Name() string
	Run(spec *config.Spec) (SectionResult, error)
}

// Walker drives a sequence of sections.
//
// The Out writer receives the final JSON document (with a trailing newline).
// Progress, when non-nil, receives human-readable progress lines as sections
// start and complete. In production both Out and Progress point to the same
// SSH stdout so operators see real-time feedback before the final JSON.
//
// Sections, when nil, is auto-populated at the top of Install/Uninstall using
// DefaultInstallSections / DefaultUninstallSections with a distro detected from
// the spec. Set it explicitly (e.g. in tests) to override the default list.
type Walker struct {
	Out      io.Writer
	Progress io.Writer
	Sections []Section
}

func (w *Walker) progress() io.Writer {
	if w.Progress != nil {
		return w.Progress
	}
	return io.Discard
}

// Install runs each section in order and stops on the first error.
//
// Distro is detected once from spec at the top of the walk (auto-detected via
// /etc/os-release when spec.Distro is empty, or resolved from the spec value
// when non-empty). The detected distro is forwarded to DefaultInstallSections
// when w.Sections is nil; injected section lists (used by tests) are run as-is.
//
// On success it writes the success-shape JSON document and returns nil.
// On failure it writes the error-shape JSON document (including
// sections_so_far) and returns an error so the caller can exit non-zero.
func (w *Walker) Install(spec *config.Spec) error {
	sections := w.Sections
	if sections == nil {
		d := detectDistro(spec)
		sections = DefaultInstallSections(w.Out, d)
	}
	results := map[string]SectionResult{}
	for _, sec := range sections {
		_, _ = fmt.Fprintf(w.progress(), "clusterboxnode: section %s starting\n", sec.Name())
		res, err := sec.Run(spec)
		if err != nil {
			_, _ = fmt.Fprintf(w.progress(), "clusterboxnode: section %s failed: %v\n", sec.Name(), err)
			return w.emitError(sec.Name(), err, results)
		}
		_, _ = fmt.Fprintf(w.progress(), "clusterboxnode: section %s done (applied=%v)\n", sec.Name(), res.Applied)
		results[sec.Name()] = res
	}
	return w.emitSuccess(results)
}

// Uninstall runs every section, recording errors onto each section's result
// instead of stopping the walk.
//
// Distro is detected once from spec at the top of the walk, mirroring
// Walker.Install. When w.Sections is nil, DefaultUninstallSections is
// called with the detected distro.
//
// The success-shape JSON document is always emitted. Returns nil unless the
// JSON encode itself fails — uninstall fails-soft per section by design.
func (w *Walker) Uninstall(spec *config.Spec) error {
	sections := w.Sections
	if sections == nil {
		d := detectDistro(spec)
		sections = DefaultUninstallSections(d)
	}
	results := map[string]SectionResult{}
	for _, sec := range sections {
		_, _ = fmt.Fprintf(w.progress(), "clusterboxnode: section %s starting\n", sec.Name())
		res, err := sec.Run(spec)
		if err != nil {
			res.Applied = false
			res.Error = err.Error()
			_, _ = fmt.Fprintf(w.progress(), "clusterboxnode: section %s error: %v\n", sec.Name(), err)
		} else {
			_, _ = fmt.Fprintf(w.progress(), "clusterboxnode: section %s done (applied=%v)\n", sec.Name(), res.Applied)
		}
		results[sec.Name()] = res
	}
	return w.emitSuccess(results)
}

func (w *Walker) emitSuccess(results map[string]SectionResult) error {
	doc := map[string]any{"sections": results}
	return w.encode(doc)
}

func (w *Walker) emitError(section string, err error, soFar map[string]SectionResult) error {
	doc := map[string]any{
		"error":           err.Error(),
		"section":         section,
		"sections_so_far": soFar,
	}
	if encErr := w.encode(doc); encErr != nil {
		return encErr
	}
	return fmt.Errorf("section %s failed: %w", section, err)
}

func (w *Walker) encode(doc map[string]any) error {
	enc := json.NewEncoder(w.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// DefaultInstallSections returns the ordered list of sections used by
// `clusterboxnode install`: harden → tailscale → k3s.
//
// d is the resolved Distro for this run (detected once by the Walker before
// sections execute). out is the writer for k3s progress lines (installer
// download, kubeconfig wait). Pass io.Discard in tests to suppress output.
func DefaultInstallSections(out io.Writer, d distro.Distro) []Section {
	return []Section{
		hardenInstallSection{Distro: d},
		tailscaleInstallSection{},
		k3sInstallSection{Out: out},
	}
}

// DefaultUninstallSections mirrors DefaultInstallSections in reverse order:
// k3s → tailscale → harden.
func DefaultUninstallSections(d distro.Distro) []Section {
	return []Section{
		k3sUninstallSection{},
		tailscaleUninstallSection{},
		hardenUninstallSection{Distro: d},
	}
}

// k3sInstallSection adapts [k3s.Section.Apply] to the walker's Section
// interface. The zero-valued k3s.Section uses real os/exec + os filesystem
// access; tests substitute the underlying section by overriding
// DefaultInstallSections in the cmd binary or by injecting their own list.
type k3sInstallSection struct {
	Out io.Writer
}

func (k3sInstallSection) Name() string { return "k3s" }

func (s k3sInstallSection) Run(spec *config.Spec) (SectionResult, error) {
	sec := &k3s.Section{Out: s.Out}
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		return SectionResult{}, err
	}
	return SectionResult{Applied: res.Applied, Reason: res.Reason, Extra: res.Extra}, nil
}

// k3sUninstallSection adapts [k3s.Section.Remove] to the walker.
type k3sUninstallSection struct{}

func (k3sUninstallSection) Name() string { return "k3s" }

func (k3sUninstallSection) Run(spec *config.Spec) (SectionResult, error) {
	sec := &k3s.Section{}
	res, err := sec.Remove(context.Background(), spec)
	if err != nil {
		return SectionResult{}, err
	}
	return SectionResult{Applied: res.Applied, Reason: res.Reason, Extra: res.Extra}, nil
}

// hardenInstallSection adapts [harden.Section.Apply] to the walker. The
// zero-valued harden.Section pulls in the real os/exec runners and
// /-rooted FS for each of its subsystems (user, sshd, ufw).
// Distro, when non-nil, is forwarded to harden.Section so that subsections
// that are distro-aware (ufw, fail2ban, auditd, unattended) receive it.
type hardenInstallSection struct {
	Distro distro.Distro
}

func (hardenInstallSection) Name() string { return "harden" }

func (s hardenInstallSection) Run(spec *config.Spec) (SectionResult, error) {
	sec := &harden.Section{Distro: s.Distro}
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		return SectionResult{}, err
	}
	return SectionResult{Applied: res.Applied, Reason: res.Reason, Extra: res.Extra}, nil
}

// hardenUninstallSection adapts [harden.Section.Remove] to the walker.
type hardenUninstallSection struct {
	Distro distro.Distro
}

func (hardenUninstallSection) Name() string { return "harden" }

func (s hardenUninstallSection) Run(spec *config.Spec) (SectionResult, error) {
	sec := &harden.Section{Distro: s.Distro}
	res, err := sec.Remove(context.Background(), spec)
	if err != nil {
		return SectionResult{}, err
	}
	return SectionResult{Applied: res.Applied, Reason: res.Reason, Extra: res.Extra}, nil
}

// tailscaleInstallSection adapts [tailscale.Section.Apply] to the walker.
type tailscaleInstallSection struct{}

func (tailscaleInstallSection) Name() string { return "tailscale" }

func (tailscaleInstallSection) Run(spec *config.Spec) (SectionResult, error) {
	sec := &tailscale.Section{}
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		return SectionResult{}, err
	}
	return SectionResult{Applied: res.Applied, Reason: res.Reason, Extra: res.Extra}, nil
}

// tailscaleUninstallSection adapts [tailscale.Section.Remove] to the walker.
type tailscaleUninstallSection struct{}

func (tailscaleUninstallSection) Name() string { return "tailscale" }

func (tailscaleUninstallSection) Run(spec *config.Spec) (SectionResult, error) {
	sec := &tailscale.Section{}
	res, err := sec.Remove(context.Background(), spec)
	if err != nil {
		return SectionResult{}, err
	}
	return SectionResult{Applied: res.Applied, Reason: res.Reason, Extra: res.Extra}, nil
}

// osFS implements distro.FS backed by the real filesystem.
type osFS struct{}

func (osFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// detectDistro resolves the Distro for this run.
//
// If spec.Distro is non-empty it is resolved via distro.FromSpec; on an
// unrecognised value it falls through to auto-detection. Otherwise
// distro.Detect reads /etc/os-release and returns Ubuntu as the safe
// fallback on any error or unrecognised ID.
func detectDistro(spec *config.Spec) distro.Distro {
	if spec != nil && spec.Distro != "" {
		if d, ok := distro.FromSpec(spec.Distro); ok {
			return d
		}
	}
	d, err := distro.Detect(context.Background(), nil, osFS{})
	if err != nil {
		return &distro.Ubuntu{}
	}
	return d
}
