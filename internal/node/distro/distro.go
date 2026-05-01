// Package distro abstracts Linux distribution differences so that node
// subsystems can branch on distro identity without importing concrete
// types.
//
// Currently two distributions are supported:
//   - "ubuntu"  — Debian-derived, ships with ufw and apt-get.
//   - "flatcar" — Container-focused immutable OS; no apt-get, no ufw.
package distro

import "context"

// Runner is the minimal process-execution interface required by distro
// helpers. It matches [ufw.Runner] and the k3s Runner so any subsystem
// runner can be passed directly.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Distro exposes the identity and package-management capability of the
// underlying Linux distribution.
type Distro interface {
	// ID returns a lowercase identifier string. Known values:
	//   "ubuntu"  — stock Canonical Ubuntu LTS
	//   "flatcar" — Flatcar Container Linux
	ID() string

	// InstallPackage installs one or more packages using the distro's
	// native package manager. On Flatcar this is a no-op because the OS
	// image is immutable; callers should check the error rather than
	// assuming success.
	InstallPackage(ctx context.Context, runner Runner, pkgs ...string) error
}

// Ubuntu is the Distro implementation for Ubuntu / Debian systems.
type Ubuntu struct{}

// ID implements [Distro].
func (Ubuntu) ID() string { return "ubuntu" }

// InstallPackage installs packages via apt-get with a non-interactive
// frontend.
func (Ubuntu) InstallPackage(ctx context.Context, runner Runner, pkgs ...string) error {
	args := append([]string{"install", "-y", "-qq"}, pkgs...)
	_, err := runner.Run(ctx, "apt-get", args...)
	return err
}

// Flatcar is the Distro implementation for Flatcar Container Linux.
type Flatcar struct{}

// ID implements [Distro].
func (Flatcar) ID() string { return "flatcar" }

// InstallPackage is a no-op on Flatcar because the root filesystem is
// read-only and there is no package manager.
func (Flatcar) InstallPackage(_ context.Context, _ Runner, _ ...string) error {
	return nil
}
