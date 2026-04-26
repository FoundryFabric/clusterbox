// Package install implements the section walker shared by the
// `clusterboxnode install` and `clusterboxnode uninstall` subcommands.
//
// The walker iterates a fixed, ordered list of Sections (harden, tailscale,
// k3s) and accumulates a per-section result map. For install, any section
// returning an error stops the walk and an error-shape JSON document is
// emitted. For uninstall, errors are recorded onto the per-section result
// and the walk continues so as much state as possible is torn down.
package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/harden"
	"github.com/foundryfabric/clusterbox/internal/node/k3s"
)

// SectionResult captures the structured output of a single section.
//
// "applied" is the only required field; sections add additional keys
// (e.g. "version", "steps") via WithExtra. Stub sections set Applied=false
// and Reason="section not implemented yet".
type SectionResult struct {
	Applied bool                   `json:"applied"`
	Reason  string                 `json:"reason,omitempty"`
	Error   string                 `json:"error,omitempty"`
	Extra   map[string]interface{} `json:"-"`
}

// MarshalJSON flattens Extra into the top-level object so that section-
// specific keys appear alongside the standard fields.
func (r SectionResult) MarshalJSON() ([]byte, error) {
	out := map[string]interface{}{}
	for k, v := range r.Extra {
		out[k] = v
	}
	out["applied"] = r.Applied
	if r.Reason != "" {
		out["reason"] = r.Reason
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
// Sections is an ordered list; callers normally use DefaultInstallSections
// or DefaultUninstallSections.
type Walker struct {
	Out      io.Writer
	Sections []Section
}

// Install runs each section in order and stops on the first error.
//
// On success it writes the success-shape JSON document and returns nil.
// On failure it writes the error-shape JSON document (including
// sections_so_far) and returns an error so the caller can exit non-zero.
func (w *Walker) Install(spec *config.Spec) error {
	results := map[string]SectionResult{}
	for _, sec := range w.Sections {
		res, err := sec.Run(spec)
		if err != nil {
			return w.emitError(sec.Name(), err, results)
		}
		results[sec.Name()] = res
	}
	return w.emitSuccess(results)
}

// Uninstall runs every section, recording errors onto each section's result
// instead of stopping the walk.
//
// The success-shape JSON document is always emitted. Returns nil unless the
// JSON encode itself fails — uninstall fails-soft per section by design.
func (w *Walker) Uninstall(spec *config.Spec) error {
	results := map[string]SectionResult{}
	for _, sec := range w.Sections {
		res, err := sec.Run(spec)
		if err != nil {
			res.Applied = false
			res.Error = err.Error()
		}
		results[sec.Name()] = res
	}
	return w.emitSuccess(results)
}

func (w *Walker) emitSuccess(results map[string]SectionResult) error {
	doc := map[string]interface{}{"sections": results}
	return w.encode(doc)
}

func (w *Walker) emitError(section string, err error, soFar map[string]SectionResult) error {
	doc := map[string]interface{}{
		"error":           err.Error(),
		"section":         section,
		"sections_so_far": soFar,
	}
	if encErr := w.encode(doc); encErr != nil {
		return encErr
	}
	return fmt.Errorf("section %s failed: %w", section, err)
}

func (w *Walker) encode(doc map[string]interface{}) error {
	enc := json.NewEncoder(w.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// stubSection is the placeholder implementation used for harden, tailscale,
// and k3s until T3-T5 fill them in.
type stubSection struct{ name string }

func (s stubSection) Name() string { return s.name }

func (s stubSection) Run(_ *config.Spec) (SectionResult, error) {
	return SectionResult{
		Applied: false,
		Reason:  "section not implemented yet",
	}, nil
}

// DefaultInstallSections returns the ordered list of sections used by
// `clusterboxnode install`.
//
// tailscale remains a stub until T5 lands; harden (T4a) and k3s (T3)
// are real implementations.
func DefaultInstallSections() []Section {
	return []Section{
		hardenInstallSection{},
		stubSection{name: "tailscale"},
		k3sInstallSection{},
	}
}

// DefaultUninstallSections mirrors DefaultInstallSections but in reverse
// order so teardown happens in the opposite order from install.
func DefaultUninstallSections() []Section {
	return []Section{
		k3sUninstallSection{},
		stubSection{name: "tailscale"},
		hardenUninstallSection{},
	}
}

// k3sInstallSection adapts [k3s.Section.Apply] to the walker's Section
// interface. The zero-valued k3s.Section uses real os/exec + os filesystem
// access; tests substitute the underlying section by overriding
// DefaultInstallSections in the cmd binary or by injecting their own list.
type k3sInstallSection struct{}

func (k3sInstallSection) Name() string { return "k3s" }

func (k3sInstallSection) Run(spec *config.Spec) (SectionResult, error) {
	sec := &k3s.Section{}
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
type hardenInstallSection struct{}

func (hardenInstallSection) Name() string { return "harden" }

func (hardenInstallSection) Run(spec *config.Spec) (SectionResult, error) {
	sec := &harden.Section{}
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		return SectionResult{}, err
	}
	return SectionResult{Applied: res.Applied, Reason: res.Reason, Extra: res.Extra}, nil
}

// hardenUninstallSection adapts [harden.Section.Remove] to the walker.
// Remove is a no-op for v1; T4b will revisit teardown semantics.
type hardenUninstallSection struct{}

func (hardenUninstallSection) Name() string { return "harden" }

func (hardenUninstallSection) Run(spec *config.Spec) (SectionResult, error) {
	sec := &harden.Section{}
	res, err := sec.Remove(context.Background(), spec)
	if err != nil {
		return SectionResult{}, err
	}
	return SectionResult{Applied: res.Applied, Reason: res.Reason, Extra: res.Extra}, nil
}
