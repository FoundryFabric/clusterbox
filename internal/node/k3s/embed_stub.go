//go:build !k3s_assets

package k3s

// EmbeddedBinary is nil in normal (non-asset) builds. The k3s_assets build
// tag must be set (along with the target arch tag) to embed the real binary.
// Use `make k3s-assets` to fetch the binaries before building with that tag.
var EmbeddedBinary []byte
