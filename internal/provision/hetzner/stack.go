package hetzner

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
	hcloudsdk "github.com/hetznercloud/hcloud-go/v2/hcloud"
)

const (
	defaultServerType = "cpx21"
	volumeSizeGB      = 100
	volumeFormat      = "ext4"
)

// CreateResult holds the IDs and IP of the resources created for one node.
type CreateResult struct {
	ServerID   int64
	ServerIPv4 string
	VolumeID   int64
	FirewallID int64
}

// OnResourceCreated is called immediately after each Hetzner resource is
// successfully created or found to already exist. resourceType and hetznerID
// identify the resource; hostname is the human-readable name.
// Errors returned by the callback are logged but do not abort provisioning.
type OnResourceCreated func(resourceType registry.HetznerResourceType, hetznerID, hostname string)

// CreateClusterResources provisions all Hetzner Cloud resources for one node
// in a get-or-create fashion: if a resource with the expected name already
// exists it is reused, so re-running after a partial failure is safe.
//
// Creation order: firewall → server → volume → volume attachment.
// onCreated is called after each resource is successfully obtained (new or
// existing) so the caller can record it in the local registry. It may be nil.
func CreateClusterResources(ctx context.Context, client *hcloudsdk.Client, cfg provision.ClusterConfig, userData string, onCreated OnResourceCreated) (CreateResult, error) {
	clusterLabel := cfg.EffectiveClusterLabel()

	notify := func(rt registry.HetznerResourceType, id int64, hostname string) {
		if onCreated != nil {
			onCreated(rt, strconv.FormatInt(id, 10), hostname)
		}
	}

	// 1. Firewall — get or create
	fwName := cfg.ClusterName + "-fw"
	var fwID int64
	existingFW, _, err := client.Firewall.GetByName(ctx, fwName)
	if err != nil {
		return CreateResult{}, fmt.Errorf("provision: lookup firewall: %w", err)
	}
	if existingFW != nil {
		fwID = existingFW.ID
	} else {
		fwLabels := StandardLabels(clusterLabel, "node-firewall")
		allIPv4 := net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(0, 32)}
		allIPv6 := net.IPNet{IP: net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, Mask: net.CIDRMask(0, 128)}
		allIPs := []net.IPNet{allIPv4, allIPv6}
		port443 := "443"
		port41641 := "41641"
		fwResult, _, err := client.Firewall.Create(ctx, hcloudsdk.FirewallCreateOpts{
			Name:   fwName,
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
		fwID = fwResult.Firewall.ID
	}
	notify(registry.ResourceFirewall, fwID, fwName)

	// 2. Server — get or create, attach firewall at creation time
	var serverID int64
	var serverIPv4 string
	existingSrv, _, err := client.Server.GetByName(ctx, cfg.ClusterName)
	if err != nil {
		return CreateResult{}, fmt.Errorf("provision: lookup server: %w", err)
	}
	if existingSrv != nil {
		serverID = existingSrv.ID
		serverIPv4 = existingSrv.PublicNet.IPv4.IP.String()
	} else {
		srvType := cfg.ServerType
		if srvType == "" {
			srvType = defaultServerType
		}
		serverLabels := StandardLabels(clusterLabel, cfg.ResourceRole)
		srvResult, _, err := client.Server.Create(ctx, hcloudsdk.ServerCreateOpts{
			Name:       cfg.ClusterName,
			ServerType: &hcloudsdk.ServerType{Name: srvType},
			Image:      &hcloudsdk.Image{Name: cfg.SnapshotName},
			Location:   &hcloudsdk.Location{Name: cfg.Location},
			UserData:   userData,
			Labels:     serverLabels,
			Firewalls:  []*hcloudsdk.ServerCreateFirewall{{Firewall: hcloudsdk.Firewall{ID: fwID}}},
		})
		if err != nil {
			return CreateResult{}, fmt.Errorf("provision: create server: %w", err)
		}
		if err := client.Action.WaitFor(ctx, srvResult.NextActions...); err != nil {
			return CreateResult{}, fmt.Errorf("provision: wait for server: %w", err)
		}
		serverID = srvResult.Server.ID
		serverIPv4 = srvResult.Server.PublicNet.IPv4.IP.String()
	}
	notify(registry.ResourceServer, serverID, cfg.ClusterName)

	// 3. Volume — get or create
	volName := cfg.ClusterName + "-data"
	var volID int64
	existingVol, _, err := client.Volume.GetByName(ctx, volName)
	if err != nil {
		return CreateResult{}, fmt.Errorf("provision: lookup volume: %w", err)
	}
	if existingVol != nil {
		volID = existingVol.ID
	} else {
		volLabels := StandardLabels(clusterLabel, "node-data")
		volLabels["role"] = "data"
		volResult, _, err := client.Volume.Create(ctx, hcloudsdk.VolumeCreateOpts{
			Name:     volName,
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
		volID = volResult.Volume.ID

		// 4. Attach volume (Automount=false — cloud-init handles the /data mount).
		// Only attach when freshly created; an existing volume is already attached.
		attachAction, _, err := client.Volume.AttachWithOpts(ctx, volResult.Volume, hcloudsdk.VolumeAttachOpts{
			Server:    &hcloudsdk.Server{ID: serverID},
			Automount: hcloudsdk.Ptr(false),
		})
		if err != nil {
			return CreateResult{}, fmt.Errorf("provision: attach volume: %w", err)
		}
		if err := client.Action.WaitFor(ctx, attachAction); err != nil {
			return CreateResult{}, fmt.Errorf("provision: wait for volume attach: %w", err)
		}
	}
	notify(registry.ResourceVolume, volID, volName)

	return CreateResult{
		ServerID:   serverID,
		ServerIPv4: serverIPv4,
		VolumeID:   volID,
		FirewallID: fwID,
	}, nil
}
