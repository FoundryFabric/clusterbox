// Package distro defines the Distro interface and shared types used to
// abstract over Linux distribution differences within clusterboxnode.
//
// Currently two distributions are supported:
//   - Ubuntu (and any Debian-compatible distro): packages installed via apt-get.
//   - Flatcar Container Linux: immutable rootfs; InstallPackage returns ErrNotSupported.
//
// Use Detect to auto-detect the running distribution from /etc/os-release, or
// FromSpec to resolve an explicit distro name from the node Spec.
package distro

import (
	"context"
	"errors"
)

// ErrNotSupported is returned by InstallPackage implementations that do not
// support runtime package installation (e.g. Flatcar).
var ErrNotSupported = errors.New("distro: package installation not supported on this distribution")

// Distro abstracts Linux distribution differences that affect how clusterboxnode
// sets up a node.
type Distro interface {
	// ID returns the canonical short identifier for the distribution,
	// e.g. "ubuntu" or "flatcar".
	ID() string

	// InstallPackage installs one or more packages using the distribution's
	// native package manager. Returns ErrNotSupported when the distribution
	// does not support runtime package installation.
	InstallPackage(ctx context.Context, runner Runner, pkgs ...string) error
}

// Runner abstracts process execution so unit tests can inject a fake.
//
// The signature is intentionally identical to the Runner interface used in
// other internal/node subsystems (e.g. internal/node/harden/ufw).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
}

// FS abstracts filesystem reads so tests can simulate missing or custom
// /etc/os-release files without touching the real filesystem.
type FS interface {
	ReadFile(path string) ([]byte, error)
}

// FromSpec returns the Distro for an explicit spec field value.
//
// Valid non-empty values are "ubuntu" and "flatcar". If specDistro is empty,
// (nil, false) is returned — callers should fall back to Detect in that case.
func FromSpec(specDistro string) (Distro, bool) {
	switch specDistro {
	case "ubuntu":
		return &Ubuntu{}, true
	case "flatcar":
		return &Flatcar{}, true
	default:
		return nil, false
	}
}
