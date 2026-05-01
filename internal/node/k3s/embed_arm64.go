//go:build arm64 && k3s_assets

package k3s

import _ "embed"

// EmbeddedBinary is the k3s binary for linux/arm64, embedded at build time
// when the k3s_assets build tag is set and the binary has been fetched via
// `make k3s-assets`.
//
//go:embed assets/k3s-linux-arm64
var EmbeddedBinary []byte
