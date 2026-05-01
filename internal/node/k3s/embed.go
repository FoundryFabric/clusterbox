package k3s

import _ "embed"

// EmbeddedVersion is the k3s release tag that was embedded at build time.
// It is always present (the version file is committed, initially empty).
// When EmbeddedBinary is non-empty, EmbeddedVersion must also be non-empty.
//
//go:embed assets/k3s.version
var EmbeddedVersion string
