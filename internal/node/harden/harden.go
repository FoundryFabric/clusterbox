// Package harden implements the harden install/uninstall section of
// clusterboxnode.
//
// The harden section is itself a small walker: it composes three
// subsystems — user, sshd, ufw — each with its own Apply/Remove
// contract. The coordinator runs the subsystems in a fixed order, fails
// fast on the first error, and aggregates the per-subsystem booleans
// into a single "steps" map exposed in the JSON contract:
//
//	{"sections":{"harden":{"applied":true,"steps":{
//	  "user_created":true,"sshd_locked_down":true,"ufw_enabled":true}}}}
//
// T4b will append fail2ban/auditd/etc. without changing this outer
// shape: each new subsystem appends a key under "steps".
//
// Linux-only: the binary that imports this package is built only for
// linux/{amd64,arm64} via the Makefile. Tests run on darwin because
// every operation flows through the Runner/FS interfaces.
package harden

import (
	"context"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/harden/sshd"
	"github.com/foundryfabric/clusterbox/internal/node/harden/ufw"
	"github.com/foundryfabric/clusterbox/internal/node/harden/user"
)

// Result is the structured payload returned by Apply / Remove.
//
// The shape mirrors the other section packages (k3s, tailscale) so the
// install walker can translate it onto its install.SectionResult
// without needing to know about the harden subsystem split.
type Result struct {
	Applied bool
	Reason  string
	Extra   map[string]interface{}
}

// Section coordinates the user, sshd, and ufw subsystems.
//
// Each field has a working zero value: production callers can simply
// construct Section{} and rely on the embedded Apply pulling in the
// real os/exec runners. Tests construct Section with subsystem-specific
// zero values overridden via the typed sub-fields.
type Section struct {
	User user.Section
	SSHD sshd.Section
	UFW  ufw.Section
}

// Apply runs the three subsystems in order: user, sshd, ufw.
//
// Behaviour matrix:
//
//   - spec.Harden nil or Enabled=false: Applied=false, Reason="disabled".
//   - any subsystem returns an error: walk stops, the error is wrapped
//     with the subsystem name, and the partial steps map is discarded
//     (the install walker only cares about the error in this case).
//   - all subsystems succeed: Applied=true with steps{user_created,
//     sshd_locked_down, ufw_enabled} all true.
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	if h := specHarden(spec); h == nil || !h.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}

	steps := map[string]interface{}{}

	userRes, err := s.User.Apply(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	steps["user_created"] = userRes.Applied

	sshdRes, err := s.SSHD.Apply(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	steps["sshd_locked_down"] = sshdRes.Applied

	ufwRes, err := s.UFW.Apply(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	steps["ufw_enabled"] = ufwRes.Applied

	return Result{
		Applied: true,
		Extra: map[string]interface{}{
			"steps": steps,
		},
	}, nil
}

// Remove is a no-op for v1.
//
// Each subsystem's Remove is also a no-op; we keep the wrapper here so
// the section walker has a uniform Apply/Remove contract and so T4b
// can flesh out teardown semantics without touching call sites.
func (s *Section) Remove(_ context.Context, _ *config.Spec) (Result, error) {
	return Result{
		Applied: false,
		Reason:  "remove not implemented",
		Extra: map[string]interface{}{
			"steps": map[string]interface{}{
				"user_created":     false,
				"sshd_locked_down": false,
				"ufw_enabled":      false,
			},
		},
	}, nil
}

func specHarden(spec *config.Spec) *config.HardenSpec {
	if spec == nil {
		return nil
	}
	return spec.Harden
}
