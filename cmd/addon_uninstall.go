package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/spf13/cobra"
)

// addonUninstallFlags holds CLI flags for `clusterbox addon uninstall`.
type addonUninstallFlags struct {
	cluster string
	yes     bool
}

var addonUninstallF addonUninstallFlags

var addonUninstallCmd = &cobra.Command{
	Use:   "uninstall <name>",
	Short: "Uninstall an addon from a cluster",
	Long: `Uninstall removes the named addon from the target cluster by re-rendering
its manifests and running "kubectl delete --ignore-not-found", then deleting
the corresponding row from the local clusterbox registry.

By default uninstall is interactive — it prompts for confirmation before
deleting anything. Pass --yes to skip the prompt (useful in CI).`,
	Args: cobra.ExactArgs(1),
	RunE: runAddonUninstall,
}

func init() {
	addonUninstallCmd.Flags().StringVar(&addonUninstallF.cluster, "cluster", "", "Target cluster name (default: active context cluster)")
	addonUninstallCmd.Flags().BoolVar(&addonUninstallF.yes, "yes", false, "Skip the interactive confirmation prompt")
}

// runAddonUninstall is the cobra RunE handler for `clusterbox addon uninstall`.
func runAddonUninstall(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunAddonUninstall(ctx, args[0], addonUninstallF.cluster, addonUninstallF.yes,
		cmd.InOrStdin(), cmd.OutOrStdout(), AddonCmdDeps{})
}

// RunAddonUninstall executes the uninstall pipeline against the supplied
// (or default) Installer. It is exported so tests can drive it with an
// injected addonInstaller and captured I/O.
//
// When yes is false the function reads a single y/N response from in. Only
// "y" or "yes" (case-insensitive, leading/trailing whitespace stripped)
// proceed; everything else aborts with a non-error nil return so the operator
// can re-run with --yes when ready.
//
// When addonName is "gha-runner-scale-set" any registered runner scale sets
// on the cluster are included in the prompt and removed before the addon is
// uninstalled.
func RunAddonUninstall(ctx context.Context, addonName, clusterName string, yes bool,
	in io.Reader, out io.Writer, deps AddonCmdDeps,
) error {
	var err error
	clusterName, err = resolveCluster(clusterName)
	if err != nil {
		return fmt.Errorf("addon uninstall: %w", err)
	}

	// Discover runner scale sets when uninstalling the GHA addon so they can
	// be included in the prompt and cleaned up before the addon itself.
	var runnerScaleSets []registry.Deployment
	if addonName == "gha-runner-scale-set" {
		runnerScaleSets = listRunnerScaleSets(ctx, clusterName, deps.OpenRegistry)
	}

	prompt := buildUninstallPrompt(addonName, clusterName, runnerScaleSets)

	if !yes {
		ok, err := promptYesNo(in, out, prompt)
		if err != nil {
			return fmt.Errorf("addon uninstall: read confirmation: %w", err)
		}
		if !ok {
			_, _ = fmt.Fprintln(out, "uninstall aborted")
			return nil
		}
	} else if len(runnerScaleSets) > 0 {
		_, _ = fmt.Fprint(out, prompt)
	}

	if err := removeRunnerScaleSets(ctx, clusterName, runnerScaleSets, deps.OpenRegistry, deps.Runner, out); err != nil {
		return fmt.Errorf("addon uninstall: %w", err)
	}

	inst, _, cleanup, err := buildInstaller(ctx, addonName, deps)
	if err != nil {
		return fmt.Errorf("addon uninstall: %w", err)
	}
	defer cleanup()

	if err := inst.Uninstall(ctx, addonName, clusterName); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "addon %q uninstalled from cluster %q\n", addonName, clusterName)
	return nil
}

// buildUninstallPrompt constructs the confirmation prompt, listing any runner
// scale sets that will also be removed.
func buildUninstallPrompt(addonName, clusterName string, runners []registry.Deployment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Uninstall addon %q from cluster %q?", addonName, clusterName)
	if len(runners) > 0 {
		names := make([]string, len(runners))
		for i, r := range runners {
			names[i] = r.Service
		}
		fmt.Fprintf(&b, "\nThis will also remove %d runner scale set(s): %s.", len(runners), strings.Join(names, ", "))
	}
	b.WriteString(" [y/N]: ")
	return b.String()
}

// listRunnerScaleSets returns all KindRunnerScaleSet deployments for the
// cluster. Errors are swallowed — a failure to list is non-fatal; the
// uninstall proceeds and any orphaned scale sets become visible on the next
// runner list.
func listRunnerScaleSets(ctx context.Context, clusterName string, openReg func(context.Context) (registry.Registry, error)) []registry.Deployment {
	if openReg == nil {
		openReg = registry.NewRegistry
	}
	reg, err := openReg(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = reg.Close() }()

	all, err := reg.ListDeployments(ctx, clusterName)
	if err != nil {
		return nil
	}
	var out []registry.Deployment
	for _, d := range all {
		if d.Kind == registry.KindRunnerScaleSet {
			out = append(out, d)
		}
	}
	return out
}

// removeRunnerScaleSets deletes each runner scale set from Kubernetes and the
// registry. Errors are reported to out but do not abort the loop — a partial
// failure still cleans up as many scale sets as possible.
func removeRunnerScaleSets(ctx context.Context, clusterName string, scaleSets []registry.Deployment,
	openReg func(context.Context) (registry.Registry, error), runner bootstrap.CommandRunner, out io.Writer,
) error {
	if len(scaleSets) == 0 {
		return nil
	}

	if openReg == nil {
		openReg = registry.NewRegistry
	}
	reg, err := openReg(ctx)
	if err != nil {
		return fmt.Errorf("open registry for runner cleanup: %w", err)
	}
	defer func() { _ = reg.Close() }()

	cl, err := reg.GetCluster(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("get cluster %q for runner cleanup: %w", clusterName, err)
	}

	if runner == nil {
		runner = bootstrap.ExecRunner{}
	}

	for _, rs := range scaleSets {
		_, _ = fmt.Fprintf(out, "removing runner scale set %q...\n", rs.Service)
		if _, kerr := runner.Run(ctx, "kubectl",
			"--kubeconfig", cl.KubeconfigPath,
			"delete", "autoscalingrunnersets", rs.Service,
			"-n", "arc-systems", "--ignore-not-found",
		); kerr != nil {
			_, _ = fmt.Fprintf(out, "warning: kubectl delete runner scale set %q: %v\n", rs.Service, kerr)
		}
		if rerr := reg.DeleteDeployment(ctx, clusterName, rs.Service); rerr != nil && !errors.Is(rerr, registry.ErrNotFound) {
			_, _ = fmt.Fprintf(out, "warning: registry delete runner scale set %q: %v\n", rs.Service, rerr)
		}
	}
	return nil
}

// promptYesNo writes prompt to out and reads a single newline-terminated
// response from in. It returns true only when the trimmed response is "y" or
// "yes" (case-insensitive). EOF without input is treated as "no" so empty
// stdin (e.g. /dev/null) declines safely rather than erroring.
func promptYesNo(in io.Reader, out io.Writer, prompt string) (bool, error) {
	if _, err := fmt.Fprint(out, prompt); err != nil {
		return false, err
	}
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	resp := strings.ToLower(strings.TrimSpace(line))
	return resp == "y" || resp == "yes", nil
}
