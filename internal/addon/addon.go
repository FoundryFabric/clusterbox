// Package addon defines the on-disk format for clusterbox addons and exposes
// a Catalog that loads addon definitions at compile time via //go:embed.
//
// An addon directory looks like:
//
//	addons/<name>/
//	  addon.yaml       # required manifest, parsed into Addon
//	  manifests/       # k8s YAML or a single helmchart.yaml
//	    *.yaml
//	  README.md        # required human description
//
// Real installer behavior (kubectl apply / helm install) lives in package
// installer (T3); this package is purely a parsed, in-memory representation.
package addon

// Strategy identifies how an addon's manifests should be applied to the cluster.
type Strategy string

const (
	// StrategyManifests means the manifests/ directory contains plain Kubernetes
	// YAML that should be applied directly (e.g. via `kubectl apply -f`).
	StrategyManifests Strategy = "manifests"

	// StrategyHelmChart means manifests/ contains a single helmchart.yaml that
	// describes a Helm chart to install.
	StrategyHelmChart Strategy = "helmchart"

	// StrategyStaged means the addon supports multiple named modes, each with
	// its own manifest subdirectory under manifests/<mode>/. Modes that contain
	// an operators/ sub-directory are applied in two phases: operators first,
	// then a kubectl-wait poll for every HelmChart job, then instances/. The
	// supported mode names are declared in addon.yaml under "modes:".
	StrategyStaged Strategy = "staged"
)

// Valid reports whether s is a recognised manifest application strategy.
func (s Strategy) Valid() bool {
	switch s {
	case StrategyManifests, StrategyHelmChart, StrategyStaged:
		return true
	default:
		return false
	}
}

// Secret describes a single secret an addon needs at install time. The Key is
// looked up via the existing secrets backend (see internal/secrets).
type Secret struct {
	Key         string `yaml:"key"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// Addon is the parsed representation of an addon directory: addon.yaml plus
// the raw bytes of every file under manifests/ keyed by the file's path
// relative to the addon root (e.g. "manifests/deployment.yaml").
type Addon struct {
	// Fields parsed from addon.yaml.
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Strategy    Strategy `yaml:"strategy"`
	// Modes lists the supported install modes for StrategyStaged addons.
	// The first entry is the default when --mode is not supplied.
	// Ignored by StrategyManifests and StrategyHelmChart addons.
	Modes    []string `yaml:"modes,omitempty"`
	Secrets  []Secret `yaml:"secrets"`
	Requires []string `yaml:"requires"`

	// Manifests is the bundle of files under manifests/, keyed by path
	// relative to the addon root (e.g. "manifests/deployment.yaml" or
	// "manifests/helmchart.yaml"). The installer (T3) renders these.
	//
	// Keying on the relative path (rather than basename) preserves any
	// subdirectory structure an addon ships with and avoids collisions.
	Manifests map[string][]byte `yaml:"-"`
}
