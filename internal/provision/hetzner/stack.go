package hetzner

import (
	"context"
	"fmt"
	"net"

	"github.com/foundryfabric/clusterbox/internal/provision"
	hcloudsdk "github.com/hetznercloud/hcloud-go/v2/hcloud"
)

const (
	serverType   = "cx42"
	volumeSizeGB = 100
	volumeFormat = "ext4"
)

// CreateResult holds the IDs and IP of the resources created for one node.
type CreateResult struct {
	ServerID   int64
	ServerIPv4 string
	VolumeID   int64
	FirewallID int64
}

// CreateClusterResources provisions all Hetzner Cloud resources for one node:
// firewall → server (with firewall attached at creation) → volume → volume attachment.
//
// DNS record creation is intentionally omitted; Tailscale handles connectivity.
// Resources carry the standard managed-by + cluster-name labels so the reconciler
// can track them.
func CreateClusterResources(ctx context.Context, client *hcloudsdk.Client, cfg provision.ClusterConfig, userData string) (CreateResult, error) {
	clusterLabel := cfg.EffectiveClusterLabel()

	// 1. Firewall
	fwLabels := StandardLabels(clusterLabel, "node-firewall")
	allIPv4 := net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(0, 32)}
	allIPv6 := net.IPNet{IP: net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, Mask: net.CIDRMask(0, 128)}
	allIPs := []net.IPNet{allIPv4, allIPv6}
	port443 := "443"
	port41641 := "41641"
	fwResult, _, err := client.Firewall.Create(ctx, hcloudsdk.FirewallCreateOpts{
		Name:   cfg.ClusterName + "-fw",
		Labels: fwLabels,
		Rules: []hcloudsdk.FirewallRule{
			{Direction: hcloudsdk.FirewallRuleDirectionIn, Protocol: hcloudsdk.FirewallRuleProtocolTCP, Port: &port443, SourceIPs: allIPs},
			{Direction: hcloudsdk.FirewallRuleDirectionIn, Protocol: hcloudsdk.FirewallRuleProtocolUDP, Port: &port41641, SourceIPs: allIPs},
			{Direction: hcloudsdk.FirewallRuleDirectionIn, Protocol: hcloudsdk.FirewallRuleProtocolICMP, SourceIPs: allIPs},
		},
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("provision: create firewall: %w", err)
	}

	// 2. Server — attach firewall at creation time
	serverLabels := StandardLabels(clusterLabel, cfg.ResourceRole)
	srvResult, _, err := client.Server.Create(ctx, hcloudsdk.ServerCreateOpts{
		Name:       cfg.ClusterName,
		ServerType: &hcloudsdk.ServerType{Name: serverType},
		Image:      &hcloudsdk.Image{Name: cfg.SnapshotName},
		Location:   &hcloudsdk.Location{Name: cfg.Location},
		UserData:   userData,
		Labels:     serverLabels,
		Firewalls:  []*hcloudsdk.ServerCreateFirewall{{Firewall: hcloudsdk.Firewall{ID: fwResult.Firewall.ID}}},
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("provision: create server: %w", err)
	}
	if err := client.Action.WaitFor(ctx, srvResult.NextActions...); err != nil {
		return CreateResult{}, fmt.Errorf("provision: wait for server: %w", err)
	}

	// 3. Volume
	volLabels := StandardLabels(clusterLabel, "node-data")
	volLabels["role"] = "data"
	volResult, _, err := client.Volume.Create(ctx, hcloudsdk.VolumeCreateOpts{
		Name:     cfg.ClusterName + "-data",
		Size:     volumeSizeGB,
		Location: &hcloudsdk.Location{Name: cfg.Location},
		Format:   hcloudsdk.Ptr(volumeFormat),
		Labels:   volLabels,
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("provision: create volume: %w", err)
	}
	if err := client.Action.WaitFor(ctx, volResult.Action); err != nil {
		return CreateResult{}, fmt.Errorf("provision: wait for volume create: %w", err)
	}

	// 4. Attach volume (Automount=false — cloud-init handles the /data mount)
	attachAction, _, err := client.Volume.AttachWithOpts(ctx, volResult.Volume, hcloudsdk.VolumeAttachOpts{
		Server:    srvResult.Server,
		Automount: hcloudsdk.Ptr(false),
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("provision: attach volume: %w", err)
	}
	if err := client.Action.WaitFor(ctx, attachAction); err != nil {
		return CreateResult{}, fmt.Errorf("provision: wait for volume attach: %w", err)
	}

	return CreateResult{
		ServerID:   srvResult.Server.ID,
		ServerIPv4: srvResult.Server.PublicNet.IPv4.IP.String(),
		VolumeID:   volResult.Volume.ID,
		FirewallID: fwResult.Firewall.ID,
	}, nil
}
