package agentbundle_test

import (
	"testing"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/agentbundle"
)

// TestForArch_ReturnsNonEmpty asserts that both supported architectures
// resolve to a non-empty byte slice. It runs against whatever the Makefile
// has built into internal/agentbundle/agents/. When the package is built
// with plain `go test ./...` (no Makefile), the binaries may not yet exist;
// in that case the test is skipped to keep developer iteration fast — the
// CI Makefile pipeline is the authoritative gate.
func TestForArch_ReturnsNonEmpty(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			b, err := agentbundle.ForArch(arch)
			if err != nil {
				t.Skipf("embedded clusterboxnode-linux-%s not present (run via Makefile): %v", arch, err)
			}
			if len(b) == 0 {
				t.Fatalf("ForArch(%q) returned 0 bytes", arch)
			}
			// ELF magic — embedded binaries must be linux ELFs, never the
			// host's Mach-O/PE artefacts. Catches a Makefile that forgot
			// GOOS=linux on the cross-compile.
			if b[0] != 0x7f || b[1] != 'E' || b[2] != 'L' || b[3] != 'F' {
				t.Fatalf("ForArch(%q): expected ELF magic, got %x %x %x %x",
					arch, b[0], b[1], b[2], b[3])
			}
		})
	}
}

func TestForArch_RejectsUnknownArch(t *testing.T) {
	cases := []string{"", "386", "riscv64", "windows-amd64", "darwin-arm64"}
	for _, arch := range cases {
		if _, err := agentbundle.ForArch(arch); err == nil {
			t.Errorf("ForArch(%q) returned nil error; want unsupported-arch error", arch)
		}
	}
}

// TestVersion_MatchesClusterbox is the version-tripwire: it fails loudly if
// the Makefile passes different -X version=... flags to the clusterbox and
// clusterboxnode builds, which would mean the embedded agent is from a
// different commit than the surrounding CLI. With plain `go test` (no
// Makefile), both fall back to the "dev" default, so this still passes.
func TestVersion_MatchesClusterbox(t *testing.T) {
	if got, want := agentbundle.Version(), cmd.Version(); got != want {
		t.Fatalf("agentbundle.Version() = %q, cmd.Version() = %q — Makefile ldflags out of sync", got, want)
	}
}
