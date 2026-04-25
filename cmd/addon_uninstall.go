package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
	addonUninstallCmd.Flags().StringVar(&addonUninstallF.cluster, "cluster", "", "Target cluster name (required)")
	_ = addonUninstallCmd.MarkFlagRequired("cluster")
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
func RunAddonUninstall(ctx context.Context, addonName, clusterName string, yes bool,
	in io.Reader, out io.Writer, deps AddonCmdDeps,
) error {
	if clusterName == "" {
		return fmt.Errorf("addon uninstall: --cluster is required")
	}

	if !yes {
		ok, err := promptYesNo(in, out,
			fmt.Sprintf("Uninstall addon %q from cluster %q? [y/N]: ", addonName, clusterName))
		if err != nil {
			return fmt.Errorf("addon uninstall: read confirmation: %w", err)
		}
		if !ok {
			fmt.Fprintln(out, "uninstall aborted")
			return nil
		}
	}

	inst, _, cleanup, err := buildInstaller(ctx, addonName, deps)
	if err != nil {
		return fmt.Errorf("addon uninstall: %w", err)
	}
	defer cleanup()

	if err := inst.Uninstall(ctx, addonName, clusterName); err != nil {
		return err
	}
	fmt.Fprintf(out, "addon %q uninstalled from cluster %q\n", addonName, clusterName)
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
