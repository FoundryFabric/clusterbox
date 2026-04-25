// Copyright 2026 Foundry Fabric

package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sync"
)

// kubeJSONMulti builds a kubectl deployment list JSON document with the
// supplied items. It complements the single-item kubeJSON helper in
// sync_test.go, which is fixed to one (svc, version) pair.
func kubeJSONMulti(items ...kubeDiffDep) []byte {
	type item struct {
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels,omitempty"`
		} `json:"metadata"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			Replicas          int32 `json:"replicas"`
			ReadyReplicas     int32 `json:"readyReplicas"`
			UpdatedReplicas   int32 `json:"updatedReplicas"`
			AvailableReplicas int32 `json:"availableReplicas"`
		} `json:"status"`
	}
	type listEnv struct {
		Items []item `json:"items"`
	}
	env := listEnv{}
	for _, d := range items {
		var it item
		it.Metadata.Name = d.name
		it.Metadata.Namespace = d.namespace
		if d.labelName != "" {
			it.Metadata.Labels = map[string]string{"app.kubernetes.io/name": d.labelName}
		}
		it.Spec.Template.Spec.Containers = []struct {
			Image string `json:"image"`
		}{{Image: d.image}}
		it.Status.Replicas = d.replicas
		it.Status.ReadyReplicas = d.ready
		it.Status.UpdatedReplicas = d.updated
		it.Status.AvailableReplicas = d.available
		env.Items = append(env.Items, it)
	}
	out, _ := json.Marshal(env)
	return out
}

type kubeDiffDep struct {
	name      string
	namespace string
	labelName string
	image     string
	replicas  int32
	ready     int32
	updated   int32
	available int32
}

// seedDiffCluster inserts a cluster plus optional nodes and deployments
// into reg.
func seedDiffCluster(t *testing.T, reg registry.Registry, c registry.Cluster, nodes []registry.Node, deps []registry.Deployment) {
	t.Helper()
	ctx := context.Background()
	if err := reg.UpsertCluster(ctx, c); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	for _, n := range nodes {
		if err := reg.UpsertNode(ctx, n); err != nil {
			t.Fatalf("seed node: %v", err)
		}
	}
	for _, d := range deps {
		if err := reg.UpsertDeployment(ctx, d); err != nil {
			t.Fatalf("seed deployment: %v", err)
		}
	}
}

func runDiffForTest(t *testing.T, reg registry.Registry, pulumi sync.PulumiClient, kubectl sync.KubectlRunner, cluster string, asJSON bool) (string, string, error) {
	t.Helper()
	deps := cmd.DiffDeps{
		OpenRegistry: func(_ context.Context) (registry.Registry, error) { return reg, nil },
		Pulumi:       pulumi,
		Kubectl:      kubectl,
	}
	var stdout, stderr bytes.Buffer
	err := cmd.RunDiff(context.Background(), cluster, deps, &stdout, &stderr, asJSON)
	return stdout.String(), stderr.String(), err
}

func TestDiff_NoDrift_ExitsZero(t *testing.T) {
	reg := newRegistry(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cluster := registry.Cluster{
		Name:           "prod-ash",
		Provider:       "hetzner",
		Region:         "ash",
		Env:            "prod",
		CreatedAt:      now,
		KubeconfigPath: "/tmp/kubeconfig",
		LastSynced:     now,
	}
	nodes := []registry.Node{{ClusterName: "prod-ash", Hostname: "prod-ash", Role: "control-plane", JoinedAt: now}}
	deps := []registry.Deployment{{ClusterName: "prod-ash", Service: "svc-a", Version: "v1", DeployedAt: now, DeployedBy: "chris", Status: registry.StatusRolledOut}}
	seedDiffCluster(t, reg, cluster, nodes, deps)

	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"prod-ash": {{Hostname: "prod-ash", Role: "control-plane"}},
	}}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/tmp/kubeconfig": kubeJSONMulti(kubeDiffDep{
			name: "svc-a", namespace: "default", image: "registry/svc-a:v1",
			replicas: 1, ready: 1, updated: 1, available: 1,
		}),
	}}

	stdout, _, err := runDiffForTest(t, reg, pulumi, kctl, "prod-ash", false)
	if err != nil {
		t.Fatalf("expected nil error for no drift, got %v", err)
	}
	if !strings.Contains(stdout, "no drift") {
		t.Errorf("expected 'no drift' in output, got:\n%s", stdout)
	}
}

func TestDiff_VersionMismatch_ExitsOne(t *testing.T) {
	reg := newRegistry(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cluster := registry.Cluster{Name: "prod-ash", Provider: "hetzner", Region: "ash", Env: "prod", CreatedAt: now, KubeconfigPath: "/tmp/kubeconfig", LastSynced: now}
	seedDiffCluster(t, reg, cluster, nil, []registry.Deployment{
		{ClusterName: "prod-ash", Service: "svc-a", Version: "v1", DeployedAt: now, DeployedBy: "chris", Status: registry.StatusRolledOut},
	})

	pulumi := &fakePulumi{}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/tmp/kubeconfig": kubeJSONMulti(kubeDiffDep{
			name: "svc-a", namespace: "default", image: "registry/svc-a:v2",
			replicas: 1, ready: 1, updated: 1, available: 1,
		}),
	}}

	stdout, _, err := runDiffForTest(t, reg, pulumi, kctl, "prod-ash", false)
	if err == nil {
		t.Fatalf("expected drift error, got nil")
	}
	if !strings.Contains(err.Error(), "drift detected") {
		t.Errorf("expected drift-detected error, got %v", err)
	}
	if !strings.Contains(stdout, "~ svc-a") {
		t.Errorf("expected '~ svc-a' in output, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "v1 → v2") {
		t.Errorf("expected version arrow 'v1 → v2', got:\n%s", stdout)
	}
}

func TestDiff_AddedAndRemoved(t *testing.T) {
	reg := newRegistry(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cluster := registry.Cluster{Name: "prod-ash", Provider: "hetzner", Region: "ash", Env: "prod", CreatedAt: now, KubeconfigPath: "/tmp/kubeconfig"}
	seedDiffCluster(t, reg, cluster,
		[]registry.Node{{ClusterName: "prod-ash", Hostname: "prod-ash", Role: "control-plane", JoinedAt: now}},
		[]registry.Deployment{{ClusterName: "prod-ash", Service: "gone-svc", Version: "v1", DeployedAt: now, Status: registry.StatusRolledOut}},
	)

	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"prod-ash": {
			{Hostname: "prod-ash", Role: "control-plane"},
			{Hostname: "prod-ash-node-1", Role: "worker"},
		},
	}}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/tmp/kubeconfig": kubeJSONMulti(kubeDiffDep{
			name: "new-svc", namespace: "default", image: "registry/new-svc:v1",
			replicas: 1, ready: 1, updated: 1, available: 1,
		}),
	}}

	stdout, _, err := runDiffForTest(t, reg, pulumi, kctl, "prod-ash", false)
	if err == nil {
		t.Fatalf("expected drift, got nil error")
	}
	if !strings.Contains(stdout, "+ new-svc") {
		t.Errorf("expected added 'new-svc', got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "- gone-svc") {
		t.Errorf("expected removed 'gone-svc', got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "+ prod-ash-node-1") {
		t.Errorf("expected added node 'prod-ash-node-1', got:\n%s", stdout)
	}
}

func TestDiff_ClusterNotFound(t *testing.T) {
	reg := newRegistry(t)
	pulumi := &fakePulumi{}
	kctl := &fakeKubectl{}

	_, _, err := runDiffForTest(t, reg, pulumi, kctl, "missing", false)
	if err == nil {
		t.Fatal("expected error for unknown cluster, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %v", err)
	}
}

// erroringKubectl always returns an error from Run. Used to test the
// error-path exit code in the diff command.
type erroringKubectl struct {
	err error
}

func (e *erroringKubectl) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return nil, e.err
}

func TestDiff_KubectlError(t *testing.T) {
	reg := newRegistry(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	seedDiffCluster(t, reg,
		registry.Cluster{Name: "prod-ash", Provider: "hetzner", Region: "ash", Env: "prod", CreatedAt: now, KubeconfigPath: "/tmp/kubeconfig"},
		nil, nil,
	)
	pulumi := &fakePulumi{}
	kctl := &erroringKubectl{err: errors.New("connection refused")}

	_, _, err := runDiffForTest(t, reg, pulumi, kctl, "prod-ash", false)
	if err == nil {
		t.Fatal("expected kubectl error, got nil")
	}
	if !strings.Contains(err.Error(), "kubectl") {
		t.Errorf("expected 'kubectl' in error, got %v", err)
	}
}

func TestDiff_JSONOutput(t *testing.T) {
	reg := newRegistry(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cluster := registry.Cluster{Name: "prod-ash", Provider: "hetzner", Region: "ash", Env: "prod", CreatedAt: now, KubeconfigPath: "/tmp/kubeconfig"}
	seedDiffCluster(t, reg, cluster, nil, []registry.Deployment{
		{ClusterName: "prod-ash", Service: "svc-a", Version: "v1", DeployedAt: now, Status: registry.StatusRolledOut},
	})

	pulumi := &fakePulumi{}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/tmp/kubeconfig": kubeJSONMulti(kubeDiffDep{
			name: "svc-a", namespace: "default", image: "registry/svc-a:v2",
			replicas: 1, ready: 1, updated: 1, available: 1,
		}),
	}}

	stdout, _, err := runDiffForTest(t, reg, pulumi, kctl, "prod-ash", true)
	if err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("expected drift error, got %v", err)
	}
	var env struct {
		Cluster string `json:"cluster"`
		Drift   bool   `json:"drift"`
		Report  struct {
			ServicesChanged []map[string]any `json:"services_changed"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.Cluster != "prod-ash" || !env.Drift {
		t.Errorf("unexpected envelope: %+v", env)
	}
	if len(env.Report.ServicesChanged) != 1 {
		t.Errorf("expected 1 services_changed entry, got %d", len(env.Report.ServicesChanged))
	}
}
