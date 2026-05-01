//go:build !tailscale_assets

package tailscale

// EmbeddedTailscale is nil when the tailscale_assets build tag is absent.
// Use "make tailscale-assets" and build with -tags tailscale_assets to embed
// the real binary.
var EmbeddedTailscale []byte

// EmbeddedTailscaled is nil when the tailscale_assets build tag is absent.
var EmbeddedTailscaled []byte
