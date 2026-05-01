//go:build amd64 && tailscale_assets

package tailscale

import _ "embed"

//go:embed assets/tailscale-linux-amd64
var EmbeddedTailscale []byte

//go:embed assets/tailscaled-linux-amd64
var EmbeddedTailscaled []byte
