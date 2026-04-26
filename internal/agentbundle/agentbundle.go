// Package agentbundle embeds the on-host clusterboxnode agent binaries into
// the clusterbox CLI so that "clusterbox up"/"add-node" flows can copy the
// matching agent to a target host without a separate download step.
//
// Two binaries are embedded — linux/amd64 and linux/arm64 — built from the
// exact same source tree as the surrounding clusterbox binary by the
// Makefile. agentbundle.Version() carries the same version string as the
// clusterbox binary, and a unit test asserts the two stay in lock-step so a
// Makefile bug that mismatches them fails loudly.
package agentbundle

import (
	"embed"
	"fmt"
	"io/fs"
)

// agents holds the cross-compiled clusterboxnode binaries. The Makefile
// produces these files into ./agents/ before "go build" runs on clusterbox,
// so the embed directive always sees the freshly-built artefacts.
//
// We name the two files explicitly (rather than using a glob) so that a
// missing binary fails the build immediately with a clear "pattern X
// matches no files" error from the Go toolchain — making Makefile drift
// impossible to miss.
//
//go:embed agents/clusterboxnode-linux-amd64
//go:embed agents/clusterboxnode-linux-arm64
var agents embed.FS

// ForArch returns the embedded clusterboxnode binary bytes for the given
// linux architecture. Supported values are "amd64" and "arm64"; any other
// value returns an error.
func ForArch(arch string) ([]byte, error) {
	switch arch {
	case "amd64", "arm64":
	default:
		return nil, fmt.Errorf("agentbundle: unsupported arch %q (want amd64 or arm64)", arch)
	}

	name := "agents/clusterboxnode-linux-" + arch
	b, err := fs.ReadFile(agents, name)
	if err != nil {
		return nil, fmt.Errorf("agentbundle: read %s: %w", name, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("agentbundle: embedded %s is empty — Makefile likely did not build it", name)
	}
	return b, nil
}
