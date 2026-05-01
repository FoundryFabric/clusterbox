package provision

// defaultAddons maps each provider name to the ordered list of addon names
// that clusterbox installs automatically during "clusterbox up". The order
// within the list is secondary — the addon's Role field drives final install
// ordering. Providers not present in the map get no auto-installed addons.
//
// Keeping the defaults here (rather than on the Provider interface) preserves
// the CLAUDE.md rule that Provider is cloud-agnostic. Addons are a cluster
// concern, not an infrastructure lifecycle concern.
var defaultAddons = map[string][]string{
	"hetzner":   {"hcloud-ccm", "hcloud-csi", "traefik"},
	"qemu":      {"traefik"},
	"baremetal": {"traefik"},
}

// DefaultAddons returns the addon names to auto-install for providerName.
// Returns nil (not an error) for unknown providers.
func DefaultAddons(providerName string) []string {
	return append([]string(nil), defaultAddons[providerName]...)
}
