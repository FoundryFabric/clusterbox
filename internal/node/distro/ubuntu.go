package distro

import (
	"context"
	"fmt"
)

// Ubuntu is the Distro implementation for Ubuntu (and Debian-compatible)
// distributions. Packages are installed via apt-get.
type Ubuntu struct{}

// ID returns "ubuntu".
func (u *Ubuntu) ID() string { return "ubuntu" }

// InstallPackage runs:
//
//	DEBIAN_FRONTEND=noninteractive apt-get update -qq
//	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq <pkgs...>
//
// Both invocations set DEBIAN_FRONTEND=noninteractive so they never block
// waiting for tty input.
func (u *Ubuntu) InstallPackage(ctx context.Context, runner Runner, pkgs ...string) error {
	if len(pkgs) == 0 {
		return nil
	}

	env := []string{"DEBIAN_FRONTEND=noninteractive"}

	if _, err := runner.RunEnv(ctx, env, "apt-get", "update", "-qq"); err != nil {
		return fmt.Errorf("distro: apt-get update: %w", err)
	}

	args := append([]string{"install", "-y", "-qq"}, pkgs...)
	if _, err := runner.RunEnv(ctx, env, "apt-get", args...); err != nil {
		return fmt.Errorf("distro: apt-get install %v: %w", pkgs, err)
	}

	return nil
}
