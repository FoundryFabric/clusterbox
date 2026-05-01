//go:build arm64 && tailscale_assets

package tailscale

import _ "embed"

//go:embed assets/tailscale-linux-arm64
var EmbeddedTailscale []byte

//go:embed assets/tailscaled-linux-arm64
var EmbeddedTailscaled []byte
