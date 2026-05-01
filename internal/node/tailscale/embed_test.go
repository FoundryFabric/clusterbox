package tailscale_test

import (
	"testing"

	"github.com/foundryfabric/clusterbox/internal/node/tailscale"
)

// TestEmbeddedPairedInvariant verifies that:
//   - When EmbeddedTailscale is non-empty, EmbeddedVersion must also be non-empty.
//   - When EmbeddedTailscale is non-empty, EmbeddedTailscaled must also be non-empty.
//   - When EmbeddedTailscaled is non-empty, EmbeddedTailscale must also be non-empty.
//
// This test skips gracefully when no binaries are embedded (the common CI
// case). It only fails when the invariant is violated — e.g. one binary was
// embedded without the other, or the version file was left empty after a
// "make tailscale-assets" run.
func TestEmbeddedPairedInvariant(t *testing.T) {
	hasTS := len(tailscale.EmbeddedTailscale) > 0
	hasTSd := len(tailscale.EmbeddedTailscaled) > 0
	hasVer := len(tailscale.EmbeddedVersion) > 0

	if !hasTS && !hasTSd {
		t.Skip("tailscale binaries not embedded; skipping paired-invariant check (build without -tags tailscale_assets)")
	}

	if hasTS && !hasTSd {
		t.Error("EmbeddedTailscale is non-empty but EmbeddedTailscaled is nil — both must be embedded together")
	}
	if hasTSd && !hasTS {
		t.Error("EmbeddedTailscaled is non-empty but EmbeddedTailscale is nil — both must be embedded together")
	}
	if hasTS && !hasVer {
		t.Error("EmbeddedTailscale is non-empty but EmbeddedVersion is empty — run 'make tailscale-assets' to populate assets/tailscale.version")
	}
}
