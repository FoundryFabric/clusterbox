// Package harden implements the harden install/uninstall section of
// clusterboxnode.
//
// The harden section is itself a small walker: it composes seven
// subsystems in two groups — access-control (user, sshd, ufw) followed
// by defense-in-depth (fail2ban, auditd, unattended-upgrades, sysctl) —
// each with its own Apply/Remove contract. The coordinator runs them in
// a fixed order, fails fast on the first error, and aggregates the
// per-subsystem booleans into a single "steps" map:
//
//	{"sections":{"harden":{"applied":true,"steps":{
//	  "user_created":true,"sshd_locked_down":true,"ufw_enabled":true,
//	  "fail2ban_enabled":true,"auditd_enabled":true,
//	  "unattended_upgrades_enabled":true,"sysctl_applied":true}}}}
//
// Linux-only: the binary that imports this package is built only for
// linux/{amd64,arm64} via the Makefile. Tests run on darwin because
// every operation flows through the Runner/FS interfaces.
package harden

import (
	"context"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/harden/auditd"
	"github.com/foundryfabric/clusterbox/internal/node/harden/fail2ban"
	"github.com/foundryfabric/clusterbox/internal/node/harden/sshd"
	"github.com/foundryfabric/clusterbox/internal/node/harden/sysctl"
	"github.com/foundryfabric/clusterbox/internal/node/harden/ufw"
	"github.com/foundryfabric/clusterbox/internal/node/harden/unattended"
	"github.com/foundryfabric/clusterbox/internal/node/harden/user"
)

// Result is the structured payload returned by Apply / Remove.
type Result struct {
	Applied bool
	Reason  string
	Extra   map[string]interface{}
}

// Section coordinates all seven harden subsystems.
//
// Each field has a working zero value: production callers construct
// Section{} and rely on Apply pulling in real os/exec runners.
type Section struct {
	User       user.Section
	SSHD       sshd.Section
	UFW        ufw.Section
	Fail2ban   fail2ban.Section
	Auditd     auditd.Section
	Unattended unattended.Section
	Sysctl     sysctl.Section
}

// Apply runs all seven subsystems in order:
// user → sshd → ufw → fail2ban → auditd → unattended-upgrades → sysctl.
//
// Behaviour matrix:
//
//   - spec.Harden nil or Enabled=false: Applied=false, Reason="disabled".
//   - any subsystem returns an error: walk stops, error is returned.
//   - all succeed: Applied=true with all seven step keys true.
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

	fb2Res, err := s.Fail2ban.Apply(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	steps["fail2ban_enabled"] = fb2Res.Applied
	if fb2Res.Skipped {
		steps["fail2ban_skipped"] = true
	}

	auditRes, err := s.Auditd.Apply(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	steps["auditd_enabled"] = auditRes.Applied
	if auditRes.Skipped {
		steps["auditd_skipped"] = true
	}

	unaRes, err := s.Unattended.Apply(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	steps["unattended_upgrades_enabled"] = unaRes.Applied
	if unaRes.Skipped {
		steps["unattended_upgrades_skipped"] = true
	}

	sysctlRes, err := s.Sysctl.Apply(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	steps["sysctl_applied"] = sysctlRes.Applied

	return Result{
		Applied: true,
		Extra:   map[string]interface{}{"steps": steps},
	}, nil
}

// Remove is a no-op for v1. Teardown semantics are documented in T11b.
func (s *Section) Remove(_ context.Context, _ *config.Spec) (Result, error) {
	return Result{
		Applied: false,
		Reason:  "remove not implemented",
		Extra: map[string]interface{}{
			"steps": map[string]interface{}{
				"user_created":                false,
				"sshd_locked_down":            false,
				"ufw_enabled":                 false,
				"fail2ban_enabled":            false,
				"auditd_enabled":              false,
				"unattended_upgrades_enabled": false,
				"sysctl_applied":              false,
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
