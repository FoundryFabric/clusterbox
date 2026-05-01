package distro

import "context"

// Flatcar is the Distro implementation for Flatcar Container Linux.
//
// Flatcar has an immutable root filesystem; runtime package installation is
// not supported. Any call to InstallPackage returns ErrNotSupported.
type Flatcar struct{}

// ID returns "flatcar".
func (f *Flatcar) ID() string { return "flatcar" }

// InstallPackage always returns ErrNotSupported because Flatcar Container
// Linux does not support runtime package installation via a package manager.
func (f *Flatcar) InstallPackage(_ context.Context, _ Runner, _ ...string) error {
	return ErrNotSupported
}
