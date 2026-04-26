package main

// version is set at build time via `-ldflags "-X main.version=$VERSION"`,
// mirroring the existing clusterbox binary. It defaults to "dev" so that
// developer builds report a sane value without requiring any flags.
var version = "dev"
