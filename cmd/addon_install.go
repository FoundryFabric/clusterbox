package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/foundryfabric/clusterbox/internal/addon"
	"github.com/foundryfabric/clusterbox/internal/registry"
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/foundryfabric/clusterbox/internal/secrets"
	"github.com/spf13/cobra"
)

// addonInstaller is the subset of *addon.Installer that the cobra wrappers
// need. Defining it here lets tests inject a fake without wiring a catalog,
// registry, secrets resolver, and kubectl runner.
type addonInstaller interface {
	Install(ctx context.Context, addonName, clusterName, mode string) error
	Uninstall(ctx context.Context, addonName, clusterName string) error
	Upgrade(ctx context.Context, addonName, clusterName, mode string) error
}

// AddonCmdDeps groups the dependencies the addon install/uninstall/upgrade
// wrappers need. nil fields fall back to production defaults.
type AddonCmdDeps struct {
	// Installer, if non-nil, is used as-is and short-circuits the
	// catalog/registry/secrets wiring below. Tests use this to bypass the
	// real installer entirely.
	Installer addonInstaller

	// Catalog overrides the default embedded catalog. nil → addon.DefaultCatalog().
	Catalog *addon.Catalog

	// OpenRegistry overrides the registry constructor. nil → registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// NewResolver overrides the secrets resolver constructor. nil → the
	// SECRETS_BACKEND-driven secrets.NewProvider + NewResolverFromProvider.
	// Returning a nil io.Closer (second result) is allowed; the wrappers will
	// only call Close on a non-nil closer.
	NewResolver func(ctx context.Context) (secrets.Resolver, io.Closer, error)

	// Runner overrides the kubectl runner. nil → secrets.ExecCommandRunner{}.
	Runner secrets.CommandRunner
}

// addonInstallFlags holds CLI flags for `clusterbox addon install`.
type addonInstallFlags struct {
	cluster string
	mode    string
}

var addonInstallF addonInstallFlags

var addonInstallCmd = &cobra.Command{
	Use:   "install <name>",
	Short: "Install an addon onto a cluster",
	Long: `Install renders the named addon's manifests with cluster-specific secret
substitution and applies them via kubectl. On success the local clusterbox
registry records the install so "clusterbox addon list --cluster <c>" sees it.

For addons with multiple modes (e.g. telemetry), use --mode to select one.
If omitted, the addon's default mode is used.

Failures from kubectl are reported verbatim; the registry row is not written
on failure paths.`,
	Args: cobra.ExactArgs(1),
	RunE: runAddonInstall,
}

func init() {
	addonInstallCmd.Flags().StringVar(&addonInstallF.cluster, "cluster", "", "Target cluster name (required)")
	_ = addonInstallCmd.MarkFlagRequired("cluster")
	addonInstallCmd.Flags().StringVar(&addonInstallF.mode, "mode", "", "Install mode for multi-mode addons (e.g. file, full)")
}

// runAddonInstall is the cobra RunE handler for `clusterbox addon install`.
func runAddonInstall(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunAddonInstall(ctx, args[0], addonInstallF.cluster, addonInstallF.mode, cmd.OutOrStdout(), AddonCmdDeps{})
}

// RunAddonInstall executes the install pipeline against the supplied (or
// default) Installer. It is exported so tests can drive it with an injected
// addonInstaller and a captured stdout writer.
//
// mode is passed through to the installer as the selected install mode for
// staged addons; an empty string uses the addon's default mode.
//
// On success it prints a one-line confirmation including the addon's catalog
// version and the target cluster name. Failures are returned verbatim so cobra
// surfaces them on stderr.
func RunAddonInstall(ctx context.Context, addonName, clusterName, mode string, out io.Writer, deps AddonCmdDeps) error {
	if clusterName == "" {
		return fmt.Errorf("addon install: --cluster is required")
	}

	inst, version, cleanup, err := buildInstaller(ctx, addonName, deps)
	if err != nil {
		return fmt.Errorf("addon install: %w", err)
	}
	defer cleanup()

	if err := inst.Install(ctx, addonName, clusterName, mode); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "addon %q (%s) installed on cluster %q\n", addonName, version, clusterName)
	return nil
}

// buildInstaller constructs the addonInstaller from deps, opening the registry
// and secrets resolver as needed. The returned cleanup must always be invoked
// (it is a no-op when deps.Installer was supplied).
//
// version is the catalog version of addonName when known, used by callers to
// embed it in success output. It is "" when deps.Installer was supplied
// directly (the test path).
func buildInstaller(ctx context.Context, addonName string, deps AddonCmdDeps) (inst addonInstaller, version string, cleanup func(), err error) {
	cleanup = func() {}

	// Test-injected installer short-circuits all wiring.
	if deps.Installer != nil {
		return deps.Installer, "", cleanup, nil
	}

	cat := deps.Catalog
	if cat == nil {
		cat = addon.DefaultCatalog()
	}
	a, lerr := cat.Get(addonName)
	if lerr != nil {
		return nil, "", cleanup, fmt.Errorf("look up addon %q: %w", addonName, lerr)
	}

	openReg := deps.OpenRegistry
	if openReg == nil {
		openReg = registry.NewRegistry
	}
	reg, rerr := openReg(ctx)
	if rerr != nil {
		return nil, "", cleanup, fmt.Errorf("open registry: %w", rerr)
	}

	newResolver := deps.NewResolver
	if newResolver == nil {
		newResolver = defaultNewResolver
	}
	resolver, closer, serr := newResolver(ctx)
	if serr != nil {
		_ = reg.Close()
		return nil, "", cleanup, fmt.Errorf("init secrets: %w", serr)
	}

	runner := deps.Runner
	if runner == nil {
		runner = secrets.ExecCommandRunner{}
	}

	cleanup = func() {
		if cerr := reg.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: registry close: %v\n", cerr)
		}
		if closer != nil {
			if cerr := closer.Close(); cerr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: secrets close: %v\n", cerr)
			}
		}
	}

	return &addon.Installer{
		Catalog:  cat,
		Secrets:  resolver,
		Kubectl:  runner,
		Registry: reg,
	}, a.Version, cleanup, nil
}

// defaultNewResolver builds the production resolver via SECRETS_BACKEND.
// secrets.Provider has no Close, so the returned io.Closer is nil.
func defaultNewResolver(ctx context.Context) (secrets.Resolver, io.Closer, error) {
	p, err := secrets.NewProvider(ctx)
	if err != nil {
		return nil, nil, err
	}
	return secrets.NewResolverFromProvider(p), nil, nil
}
