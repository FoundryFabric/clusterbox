package k3s

import (
	"strings"
	"testing"
)

// TestEmbeddedAssetConsistency verifies that if a k3s binary is embedded, the
// version file is also present and non-empty. In normal CI builds (without the
// k3s_assets build tag) EmbeddedBinary is nil and the test is skipped.
func TestEmbeddedAssetConsistency(t *testing.T) {
	if len(EmbeddedBinary) == 0 {
		t.Skip("k3s assets not embedded (build without -tags k3s_assets)")
	}
	version := strings.TrimSpace(EmbeddedVersion)
	if version == "" {
		t.Fatal("EmbeddedBinary is non-empty but EmbeddedVersion is empty: run `make k3s-assets` before building with -tags k3s_assets")
	}
}
