// Package tailscale provides embedded Tailscale binary assets for node
// installation. The actual tailscale and tailscaled binaries are gated behind
// the tailscale_assets build tag so that normal CI builds succeed without
// requiring the binaries to be downloaded first.
//
// Use "make tailscale-assets" to fetch the binaries, then build with
// -tags tailscale_assets to include them.
package tailscale

import _ "embed"

// EmbeddedVersion is the Tailscale version string written by "make
// tailscale-assets". It is always embedded (no build tag required) so that
// callers can report the bundled version without needing the actual binaries
// present.
//
//go:embed assets/tailscale.version
var EmbeddedVersion string
