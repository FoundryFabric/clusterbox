package agentbundle

// version is set at build time via
//
//	-ldflags "-X github.com/foundryfabric/clusterbox/internal/agentbundle.version=$VERSION"
//
// by the Makefile, using the exact same $VERSION value passed to
// github.com/foundryfabric/clusterbox/cmd.version on the outer clusterbox
// binary. The two strings must match — a unit test enforces this.
//
// It defaults to "dev" so that plain `go build`/`go test` (i.e. without the
// Makefile) report a sane, predictable value.
var version = "dev"

// Version returns the version string the embedded clusterboxnode binaries
// were built with. By construction (see Makefile + version-tripwire test),
// this equals the clusterbox CLI's own version stamp.
func Version() string {
	return version
}
