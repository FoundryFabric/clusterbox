package provision_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// ---- mock infrastructure ----

// testMocks implements pulumi.MockResourceMonitor. It records every resource
// registered during the Pulumi run so tests can assert on resource shapes
// without making real API calls.
type testMocks struct {
	mu        sync.Mutex
	resources []MockedResource
}

// MockedResource captures a single resource registration for assertion.
type MockedResource struct {
	TypeToken string
	Name      string
	Inputs    resource.PropertyMap
}

func (m *testMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func (m *testMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, MockedResource{
		TypeToken: args.TypeToken,
		Name:      args.Name,
		Inputs:    args.Inputs,
	})
	m.mu.Unlock()

	// Return a predictable fake numeric-string ID and minimal outputs so that
	// downstream resources can derive integer IDs from the string.
	outputs := resource.PropertyMap{}
	id := "1"

	switch args.TypeToken {
	case "hcloud:index/server:Server":
		id = "100"
		outputs = resource.PropertyMap{
			"ipv4Address": resource.NewStringProperty("1.2.3.4"),
			"serverType":  resource.NewStringProperty("cx42"),
			"location":    resource.NewStringProperty("nbg1"),
		}
	case "hcloud:index/volume:Volume":
		id = "200"
		outputs = resource.PropertyMap{
			"size":     resource.NewNumberProperty(100),
			"format":   resource.NewStringProperty("ext4"),
			"location": resource.NewStringProperty("nbg1"),
		}
	case "hcloud:index/firewall:Firewall":
		id = "300"
	case "hcloud:index/firewallAttachment:FirewallAttachment":
		id = "301"
	case "hcloud:index/volumeAttachment:VolumeAttachment":
		id = "400"
	case "hcloud:index/zoneRecord:ZoneRecord":
		id = "500"
	}

	return id, outputs, nil
}

// recordedByType returns all resources matching the given Pulumi type token.
func (m *testMocks) recordedByType(typeToken string) []MockedResource {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MockedResource
	for _, r := range m.resources {
		if r.TypeToken == typeToken {
			out = append(out, r)
		}
	}
	return out
}

// ---- cloud-init unit tests (no Pulumi) ----

func TestRenderCloudInit_ContainsTailscaleUp(t *testing.T) {
	out, err := provision.RenderCloudInit("test-cluster", "tskey-auth-abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"tailscale up",
		"--authkey=tskey-auth-abc123",
		"--hostname=test-cluster",
		"/data",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cloud-init output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestRenderCloudInit_EmptyClusterName(t *testing.T) {
	_, err := provision.RenderCloudInit("", "tskey-auth-abc123")
	if err == nil {
		t.Fatal("expected error for empty clusterName")
	}
}

func TestRenderCloudInit_EmptyAuthKey(t *testing.T) {
	_, err := provision.RenderCloudInit("test-cluster", "")
	if err == nil {
		t.Fatal("expected error for empty authKey")
	}
}

// ---- Pulumi mock stack tests ----
// These tests use ProvisionStackWithUserData, a testable variant of
// ProvisionCluster that accepts a pre-rendered cloud-init string instead of
// calling the Tailscale API.

// TestProvisionStack_ResourceCount asserts that exactly the expected resources
// are created: 1 server, 1 volume, 1 firewall, 1 firewall-attachment,
// 1 volume-attachment, 1 DNS record.
func TestProvisionStack_ResourceCount(t *testing.T) {
	mocks := &testMocks{}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return provision.ProvisionStackWithUserData(ctx, provision.ClusterConfig{
			ClusterName:  "test-node",
			SnapshotName: "clusterbox-base-v0.1.0",
			Location:     "nbg1",
			DNSDomain:    "example.com",
		}, "#cloud-config\nruncmd: []")
	}, pulumi.WithMocks("clusterbox", "test", mocks))
	if err != nil {
		t.Fatalf("stack run failed: %v", err)
	}

	assertResourceCount(t, mocks, "hcloud:index/server:Server", 1)
	assertResourceCount(t, mocks, "hcloud:index/volume:Volume", 1)
	assertResourceCount(t, mocks, "hcloud:index/firewall:Firewall", 1)
	assertResourceCount(t, mocks, "hcloud:index/firewallAttachment:FirewallAttachment", 1)
	assertResourceCount(t, mocks, "hcloud:index/volumeAttachment:VolumeAttachment", 1)
	assertResourceCount(t, mocks, "hcloud:index/zoneRecord:ZoneRecord", 1)
}

// TestProvisionStack_ServerShape asserts that the VM is a CX42 booted from
// the named snapshot.
func TestProvisionStack_ServerShape(t *testing.T) {
	mocks := &testMocks{}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return provision.ProvisionStackWithUserData(ctx, provision.ClusterConfig{
			ClusterName:  "prod-node",
			SnapshotName: "clusterbox-base-v1.2.3",
			Location:     "fsn1",
			DNSDomain:    "example.com",
		}, "#cloud-config\nruncmd: []")
	}, pulumi.WithMocks("clusterbox", "test", mocks))
	if err != nil {
		t.Fatalf("stack run failed: %v", err)
	}

	servers := mocks.recordedByType("hcloud:index/server:Server")
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	s := servers[0]

	assertStringInput(t, s.Inputs, "serverType", "cx42")
	assertStringInput(t, s.Inputs, "image", "clusterbox-base-v1.2.3")
	assertStringInput(t, s.Inputs, "location", "fsn1")
	assertStringInput(t, s.Inputs, "name", "prod-node")
}

// TestProvisionStack_VolumeShape asserts the volume is 100 GB ext4.
func TestProvisionStack_VolumeShape(t *testing.T) {
	mocks := &testMocks{}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return provision.ProvisionStackWithUserData(ctx, provision.ClusterConfig{
			ClusterName:  "vol-test",
			SnapshotName: "clusterbox-base-v0.1.0",
			Location:     "nbg1",
			DNSDomain:    "example.com",
		}, "#cloud-config\nruncmd: []")
	}, pulumi.WithMocks("clusterbox", "test", mocks))
	if err != nil {
		t.Fatalf("stack run failed: %v", err)
	}

	vols := mocks.recordedByType("hcloud:index/volume:Volume")
	if len(vols) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(vols))
	}
	v := vols[0]

	assertNumberInput(t, v.Inputs, "size", 100)
	assertStringInput(t, v.Inputs, "format", "ext4")
	assertStringInput(t, v.Inputs, "location", "nbg1")
}

// TestProvisionStack_FirewallRules asserts the firewall allows 443 and 41641
// inbound and does NOT include a rule for port 22.
func TestProvisionStack_FirewallRules(t *testing.T) {
	mocks := &testMocks{}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return provision.ProvisionStackWithUserData(ctx, provision.ClusterConfig{
			ClusterName:  "fw-test",
			SnapshotName: "clusterbox-base-v0.1.0",
			Location:     "nbg1",
			DNSDomain:    "example.com",
		}, "#cloud-config\nruncmd: []")
	}, pulumi.WithMocks("clusterbox", "test", mocks))
	if err != nil {
		t.Fatalf("stack run failed: %v", err)
	}

	fws := mocks.recordedByType("hcloud:index/firewall:Firewall")
	if len(fws) != 1 {
		t.Fatalf("expected 1 firewall, got %d", len(fws))
	}

	rules := fws[0].Inputs["rules"]
	if !rules.IsArray() {
		t.Fatalf("firewall rules is not an array: %v", rules)
	}

	var ports []string
	for _, r := range rules.ArrayValue() {
		if !r.IsObject() {
			continue
		}
		obj := r.ObjectValue()
		if p, ok := obj["port"]; ok && p.IsString() {
			ports = append(ports, p.StringValue())
		}
	}

	assertContains(t, ports, "443", "firewall must allow port 443")
	assertContains(t, ports, "41641", "firewall must allow Tailscale port 41641")
	assertNotContains(t, ports, "22", "firewall must NOT allow port 22 from public internet")
}

// TestProvisionStack_DNSRecord asserts the DNS record type is A and targets
// the correct zone and cluster name.
func TestProvisionStack_DNSRecord(t *testing.T) {
	mocks := &testMocks{}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return provision.ProvisionStackWithUserData(ctx, provision.ClusterConfig{
			ClusterName:  "dns-test",
			SnapshotName: "clusterbox-base-v0.1.0",
			Location:     "nbg1",
			DNSDomain:    "myzone.io",
		}, "#cloud-config\nruncmd: []")
	}, pulumi.WithMocks("clusterbox", "test", mocks))
	if err != nil {
		t.Fatalf("stack run failed: %v", err)
	}

	records := mocks.recordedByType("hcloud:index/zoneRecord:ZoneRecord")
	if len(records) != 1 {
		t.Fatalf("expected 1 DNS record, got %d", len(records))
	}
	r := records[0]

	assertStringInput(t, r.Inputs, "type", "A")
	assertStringInput(t, r.Inputs, "zone", "myzone.io")
	assertStringInput(t, r.Inputs, "name", "dns-test")
}

// ---- assertion helpers ----

func assertResourceCount(t *testing.T, m *testMocks, typeToken string, want int) {
	t.Helper()
	got := m.recordedByType(typeToken)
	if len(got) != want {
		t.Errorf("resource count for %s: got %d, want %d", typeToken, len(got), want)
	}
}

func assertStringInput(t *testing.T, inputs resource.PropertyMap, key, want string) {
	t.Helper()
	v, ok := inputs[resource.PropertyKey(key)]
	if !ok {
		t.Errorf("missing input %q", key)
		return
	}
	if !v.IsString() {
		t.Errorf("input %q is not a string: %v", key, v)
		return
	}
	if got := v.StringValue(); got != want {
		t.Errorf("input %q = %q; want %q", key, got, want)
	}
}

func assertNumberInput(t *testing.T, inputs resource.PropertyMap, key string, want float64) {
	t.Helper()
	v, ok := inputs[resource.PropertyKey(key)]
	if !ok {
		t.Errorf("missing input %q", key)
		return
	}
	if !v.IsNumber() {
		t.Errorf("input %q is not a number: %v", key, v)
		return
	}
	if got := v.NumberValue(); got != want {
		t.Errorf("input %q = %v; want %v", key, got, want)
	}
}

func assertContains(t *testing.T, slice []string, val, msg string) {
	t.Helper()
	for _, s := range slice {
		if s == val {
			return
		}
	}
	t.Errorf("%s: %q not found in %v", msg, val, slice)
}

func assertNotContains(t *testing.T, slice []string, val, msg string) {
	t.Helper()
	for _, s := range slice {
		if s == val {
			t.Errorf("%s: %q unexpectedly found in %v", msg, val, slice)
			return
		}
	}
}
