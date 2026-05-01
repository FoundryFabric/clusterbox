package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/spf13/cobra"
)

// DoctorDeps groups injectable dependencies for the doctor command.
type DoctorDeps struct {
	OpenRegistry func(ctx context.Context) (registry.Registry, error)
	Runner       bootstrap.CommandRunner
}

type doctorFlags struct {
	cluster string
	fix     bool
}

var doctorF doctorFlags

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check cluster and runner health, optionally auto-fix issues",
	Long: `Doctor runs a suite of health checks against the cluster and its GitHub
Actions runner infrastructure:

  • Cluster reachability
  • Node conditions (MemoryPressure, DiskPressure, PIDPressure)
  • ARC controller version and readiness
  • Runner pod health (CrashLoopBackOff detection)
  • Runner scale-set concurrency (warns when maxRunners > 1 on a single node)

With --fix, doctor auto-remediates issues it knows how to solve:
  • Restarts a crashed ARC controller deployment
  • Patches runner scale sets with excessive maxRunners down to 1`,
	Example: `  clusterbox doctor
  clusterbox doctor --cluster production
  clusterbox doctor --fix`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().StringVar(&doctorF.cluster, "cluster", "", "Target cluster name (default: active context cluster)")
	doctorCmd.Flags().BoolVar(&doctorF.fix, "fix", false, "Auto-remediate issues where possible")
	rootCmd.AddCommand(doctorCmd)
}

// checkStatus is the result of a single doctor check.
type checkStatus int

const (
	checkOK   checkStatus = iota
	checkWarn             // non-fatal, advisory
	checkFail             // fatal, exits non-zero
)

type checkResult struct {
	name   string
	status checkStatus
	detail string
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunDoctor(ctx, doctorF.cluster, doctorF.fix, cmd.OutOrStdout(), DoctorDeps{})
}

// RunDoctor executes the full health-check suite and prints results to out.
// It is exported so tests can drive it with injected deps.
func RunDoctor(ctx context.Context, cluster string, fix bool, out io.Writer, deps DoctorDeps) error {
	var err error
	cluster, err = resolveCluster(cluster)
	if err != nil {
		return fmt.Errorf("doctor: %w", err)
	}

	openReg := deps.OpenRegistry
	if openReg == nil {
		openReg = registry.NewRegistry
	}
	reg, err := openReg(ctx)
	if err != nil {
		return fmt.Errorf("doctor: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	cl, err := reg.GetCluster(ctx, cluster)
	if err != nil {
		return fmt.Errorf("doctor: get cluster %q: %w", cluster, err)
	}

	runner := deps.Runner
	if runner == nil {
		runner = bootstrap.ExecRunner{}
	}

	_, _ = fmt.Fprintf(out, "doctor: cluster %q\n\n", cluster)

	var results []checkResult
	anyFail := false

	// Run all checks, collecting results.
	results = append(results, checkClusterReachable(ctx, runner, cl.KubeconfigPath))
	results = append(results, checkNodeConditions(ctx, runner, cl.KubeconfigPath)...)
	results = append(results, checkARCController(ctx, runner, cl.KubeconfigPath))
	results = append(results, checkRunnerPods(ctx, runner, cl.KubeconfigPath))
	results = append(results, checkRunnerConcurrency(ctx, runner, cl.KubeconfigPath, fix)...)

	// If --fix was requested, run remediations before printing results.
	if fix {
		runFixes(ctx, runner, cl.KubeconfigPath, results, out)
		// Re-run checks after fixes so the output reflects the new state.
		results = nil
		results = append(results, checkClusterReachable(ctx, runner, cl.KubeconfigPath))
		results = append(results, checkNodeConditions(ctx, runner, cl.KubeconfigPath)...)
		results = append(results, checkARCController(ctx, runner, cl.KubeconfigPath))
		results = append(results, checkRunnerPods(ctx, runner, cl.KubeconfigPath))
		results = append(results, checkRunnerConcurrency(ctx, runner, cl.KubeconfigPath, false)...)
	}

	// Print results.
	for _, r := range results {
		icon := "✓"
		switch r.status {
		case checkWarn:
			icon = "⚠"
		case checkFail:
			icon = "✗"
			anyFail = true
		}
		_, _ = fmt.Fprintf(out, "  [%s] %-35s %s\n", icon, r.name, r.detail)
	}

	_, _ = fmt.Fprintln(out)
	if anyFail {
		_, _ = fmt.Fprintln(out, "doctor: one or more checks failed — run with --fix to attempt auto-remediation")
		return fmt.Errorf("doctor: checks failed")
	}
	_, _ = fmt.Fprintln(out, "doctor: all checks passed")
	return nil
}

// runFixes applies remediations for failed/warned checks before re-running.
func runFixes(ctx context.Context, runner bootstrap.CommandRunner, kubeconfig string, results []checkResult, out io.Writer) {
	for _, r := range results {
		switch r.name {
		case "ARC controller":
			if r.status == checkFail {
				_, _ = fmt.Fprintln(out, "  → restarting ARC controller...")
				_, _ = runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
					"rollout", "restart", "deployment", "arc", "-n", "arc-systems")
				_, _ = runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
					"rollout", "status", "deployment", "arc", "-n", "arc-systems", "--timeout=90s")
			}
		}
		// Runner concurrency fixes are applied inline in checkRunnerConcurrency
		// when fix=true is passed, so no separate case is needed here.
	}
	_, _ = fmt.Fprintln(out)
}

// ── Individual checks ────────────────────────────────────────────────────────

func checkClusterReachable(ctx context.Context, runner bootstrap.CommandRunner, kubeconfig string) checkResult {
	out, err := runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfig, "cluster-info")
	if err != nil {
		return checkResult{"cluster reachable", checkFail, "cannot reach API server"}
	}
	// Extract the control-plane URL from the first line.
	first := strings.SplitN(string(out), "\n", 2)[0]
	if idx := strings.Index(first, "https://"); idx >= 0 {
		return checkResult{"cluster reachable", checkOK, strings.TrimSpace(first[idx:])}
	}
	return checkResult{"cluster reachable", checkOK, "ok"}
}

// nodeConditions is the minimal shape we need from kubectl get nodes -o json.
type nodeList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

func checkNodeConditions(ctx context.Context, runner bootstrap.CommandRunner, kubeconfig string) []checkResult {
	raw, err := runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "nodes", "-o", "json")
	if err != nil {
		return []checkResult{{"node conditions", checkFail, "kubectl get nodes failed"}}
	}

	var nl nodeList
	if err := json.Unmarshal(raw, &nl); err != nil {
		return []checkResult{{"node conditions", checkFail, "failed to parse node list"}}
	}

	var results []checkResult
	pressureTypes := map[string]bool{
		"MemoryPressure": true,
		"DiskPressure":   true,
		"PIDPressure":    true,
	}

	for _, node := range nl.Items {
		name := node.Metadata.Name
		var ready bool
		var pressures []string

		for _, c := range node.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
			}
			if pressureTypes[c.Type] && c.Status == "True" {
				pressures = append(pressures, c.Type)
			}
		}

		checkName := fmt.Sprintf("node/%s", name)
		switch {
		case !ready:
			results = append(results, checkResult{checkName, checkFail, "NotReady"})
		case len(pressures) > 0:
			results = append(results, checkResult{checkName, checkWarn, strings.Join(pressures, ", ")})
		default:
			results = append(results, checkResult{checkName, checkOK, "Ready"})
		}
	}

	if len(results) == 0 {
		return []checkResult{{"node conditions", checkWarn, "no nodes found"}}
	}
	return results
}

func checkARCController(ctx context.Context, runner bootstrap.CommandRunner, kubeconfig string) checkResult {
	raw, err := runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "deployment", "arc", "-n", "arc-systems", "-o", "json")
	if err != nil {
		return checkResult{"ARC controller", checkFail, "deployment not found — addon may not be installed"}
	}

	var dep struct {
		Spec struct {
			Replicas int `json:"replicas"`
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &dep); err != nil {
		return checkResult{"ARC controller", checkFail, "failed to parse deployment"}
	}

	ready := dep.Status.ReadyReplicas
	want := dep.Spec.Replicas

	// Extract version from image tag.
	runningVersion := "unknown"
	if len(dep.Spec.Template.Spec.Containers) > 0 {
		img := dep.Spec.Template.Spec.Containers[0].Image
		if idx := strings.LastIndex(img, ":"); idx >= 0 {
			runningVersion = img[idx+1:]
		}
	}

	detail := fmt.Sprintf("%d/%d ready, version %s", ready, want, runningVersion)
	if runningVersion != arcControllerVersion {
		detail += fmt.Sprintf(" (expected %s)", arcControllerVersion)
	}

	if ready < want {
		return checkResult{"ARC controller", checkFail, detail}
	}
	if runningVersion != arcControllerVersion {
		return checkResult{"ARC controller", checkWarn, detail}
	}
	return checkResult{"ARC controller", checkOK, detail}
}

// podList is the minimal shape for kubectl get pods -o json.
type podList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				State struct {
					Waiting *struct {
						Reason string `json:"reason"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

func checkRunnerPods(ctx context.Context, runner bootstrap.CommandRunner, kubeconfig string) checkResult {
	raw, err := runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "pods", "-n", arcRunnerNamespace, "-o", "json")
	if err != nil {
		return checkResult{"runner pods", checkWarn, "namespace not found or no pods"}
	}

	var pl podList
	if err := json.Unmarshal(raw, &pl); err != nil {
		return checkResult{"runner pods", checkFail, "failed to parse pod list"}
	}

	if len(pl.Items) == 0 {
		return checkResult{"runner pods", checkOK, "no active runners (scaled to zero)"}
	}

	var crashLooping []string
	ready := 0
	for _, pod := range pl.Items {
		isCrashing := false
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				isCrashing = true
			}
		}
		if isCrashing {
			crashLooping = append(crashLooping, pod.Metadata.Name)
		} else if pod.Status.Phase == "Running" {
			ready++
		}
	}

	if len(crashLooping) > 0 {
		return checkResult{"runner pods", checkFail,
			fmt.Sprintf("%d/%d running, crashlooping: %s", ready, len(pl.Items), strings.Join(crashLooping, ", "))}
	}
	return checkResult{"runner pods", checkOK,
		fmt.Sprintf("%d/%d running", ready, len(pl.Items))}
}

// runnerSetList is the minimal shape for kubectl get autoscalingrunnersets -o json.
type runnerSetList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			MaxRunners int `json:"maxRunners"`
			MinRunners int `json:"minRunners"`
		} `json:"spec"`
	} `json:"items"`
}

// maxRunnersThreshold is the per-scale-set limit we warn above for single-node clusters.
const maxRunnersThreshold = 1

func checkRunnerConcurrency(ctx context.Context, runner bootstrap.CommandRunner, kubeconfig string, fix bool) []checkResult {
	raw, err := runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "autoscalingrunnersets", "-n", arcRunnerNamespace, "-o", "json")
	if err != nil {
		return []checkResult{{"runner concurrency", checkWarn, "no scale sets found"}}
	}

	var rsl runnerSetList
	if err := json.Unmarshal(raw, &rsl); err != nil {
		return []checkResult{{"runner concurrency", checkFail, "failed to parse scale set list"}}
	}

	if len(rsl.Items) == 0 {
		return []checkResult{{"runner concurrency", checkOK, "no scale sets configured"}}
	}

	var results []checkResult
	for _, rs := range rsl.Items {
		name := rs.Metadata.Name
		max := rs.Spec.MaxRunners
		checkName := fmt.Sprintf("runner/%s maxRunners", name)

		if max > maxRunnersThreshold {
			detail := fmt.Sprintf("%d (recommended ≤%d for single-node)", max, maxRunnersThreshold)
			if fix {
				patch := fmt.Sprintf(`{"spec":{"maxRunners":%d}}`, maxRunnersThreshold)
				if _, err := runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfig,
					"patch", "autoscalingrunnersets", name,
					"-n", arcRunnerNamespace,
					"--type=merge", "-p", patch); err == nil {
					detail = fmt.Sprintf("patched %d → %d", max, maxRunnersThreshold)
					results = append(results, checkResult{checkName, checkOK, detail})
					continue
				}
			}
			results = append(results, checkResult{checkName, checkWarn, detail})
		} else {
			results = append(results, checkResult{checkName, checkOK, fmt.Sprintf("%d", max)})
		}
	}
	return results
}
