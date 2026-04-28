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

// CreateResult holds the IDs and IPs of the resources created for one node.
type CreateResult struct {
	ServerID   int64
	ServerIPv4 string
	VolumeID   int64
	FirewallID int64
	NetworkID  int64
	PrivateIP  string
}

// OnResourceCreated is called immediately after each Hetzner resource is
// successfully created or found to already exist. resourceType and hetznerID
// identify the resource; hostname is the human-readable name.
// Errors returned by the callback are logged but do not abort provisioning.
type OnResourceCreated func(resourceType registry.ResourceType, hetznerID, hostname string)

// clusterNetworkCIDR is the private network IP range allocated per cluster.
// Each cluster gets its own isolated Hetzner private network so nodes never
// share a broadcast domain across clusters.
const clusterNetworkCIDR = "10.0.0.0/16"

// clusterSubnetCIDR is the subnet within clusterNetworkCIDR used for node
// assignments. Hetzner auto-assigns IPs from this range at server create time.
const clusterSubnetCIDR = "10.0.1.0/24"

// locationToNetworkZone maps a Hetzner datacenter location code to its
// network zone. The zone must match the server location or Hetzner rejects
// the subnet attachment.
func locationToNetworkZone(location string) hcloudsdk.NetworkZone {
	switch location {
	case "ash":
		return hcloudsdk.NetworkZoneUSEast
	case "hil":
		return hcloudsdk.NetworkZoneUSWest
	case "sin":
		return hcloudsdk.NetworkZoneAPSouthEast
	default:
		return hcloudsdk.NetworkZoneEUCentral
	}
}

// ensureClusterNetwork gets or creates the private network for a cluster and
// returns its ID. The network is labelled with the standard clusterbox labels
// so the reconciler and destroy path can track it.
func ensureClusterNetwork(ctx context.Context, client *hcloudsdk.Client, clusterName, location, clusterLabel string, notify func(registry.ResourceType, int64, string)) (int64, error) {
	netName := clusterName + "-net"

	existing, _, err := client.Network.GetByName(ctx, netName)
	if err != nil {
		return 0, fmt.Errorf("provision: lookup network: %w", err)
	}
	if existing != nil {
		notify(registry.ResourceNetwork, existing.ID, netName)
		return existing.ID, nil
	}

	_, ipRange, _ := net.ParseCIDR(clusterNetworkCIDR)
	_, subnetRange, _ := net.ParseCIDR(clusterSubnetCIDR)
	netResult, _, err := client.Network.Create(ctx, hcloudsdk.NetworkCreateOpts{
		Name:    netName,
		IPRange: ipRange,
		Labels:  StandardLabels(clusterLabel, "cluster-network"),
		Subnets: []hcloudsdk.NetworkSubnet{
			{
				Type:        hcloudsdk.NetworkSubnetTypeCloud,
				IPRange:     subnetRange,
				NetworkZone: locationToNetworkZone(location),
			},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("provision: create network: %w", err)
	}
	notify(registry.ResourceNetwork, netResult.ID, netName)
	return netResult.ID, nil
}

// CreateClusterResources provisions all Hetzner Cloud resources for one node
// in a get-or-create fashion: if a resource with the expected name already
// exists it is reused, so re-running after a partial failure is safe.
//
// Creation order: network → firewall → server (attached to network) → volume.
// onCreated is called after each resource is successfully obtained (new or
// existing) so the caller can record it in the local registry. It may be nil.
func CreateClusterResources(ctx context.Context, client *hcloudsdk.Client, cfg provision.ClusterConfig, userData string, onCreated OnResourceCreated) (CreateResult, error) {
	clusterLabel := cfg.EffectiveClusterLabel()

	notify := func(rt registry.ResourceType, id int64, hostname string) {
		if onCreated != nil {
			onCreated(rt, strconv.FormatInt(id, 10), hostname)
		}
	}

	// 0. Private network — get or create. Every cluster always has one so
	// nodes communicate over the private network instead of Tailscale.
	networkID, err := ensureClusterNetwork(ctx, client, cfg.EffectiveClusterLabel(), cfg.Location, clusterLabel, notify)
	if err != nil {
		return CreateResult{}, err
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

	// 2. Server — get or create, attached to the private network and firewall
	var serverID int64
	var serverIPv4, privateIP string
	existingSrv, _, err := client.Server.GetByName(ctx, cfg.ClusterName)
	if err != nil {
		return CreateResult{}, fmt.Errorf("provision: lookup server: %w", err)
	}
	if existingSrv != nil {
		serverID = existingSrv.ID
		if ip := existingSrv.PublicNet.IPv4.IP; ip != nil {
			serverIPv4 = ip.String()
		}
		if len(existingSrv.PrivateNet) > 0 {
			privateIP = existingSrv.PrivateNet[0].IP.String()
		}
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
			Networks:   []*hcloudsdk.Network{{ID: networkID}},
			PublicNet: &hcloudsdk.ServerCreatePublicNet{
				EnableIPv4: !cfg.NoPublicIP,
				EnableIPv6: !cfg.NoPublicIP,
			},
		})
		if err != nil {
			return CreateResult{}, fmt.Errorf("provision: create server: %w", err)
		}
		if err := client.Action.WaitFor(ctx, srvResult.NextActions...); err != nil {
			return CreateResult{}, fmt.Errorf("provision: wait for server: %w", err)
		}
		serverID = srvResult.Server.ID
		if ip := srvResult.Server.PublicNet.IPv4.IP; ip != nil {
			serverIPv4 = ip.String()
		}
		if len(srvResult.Server.PrivateNet) > 0 {
			privateIP = srvResult.Server.PrivateNet[0].IP.String()
		}
	}
	notify(registry.ResourceServer, serverID, cfg.ClusterName)

	// 3. Volume — get or create (skipped when cfg.NoVolume is true)
	volName := cfg.ClusterName + "-data"
	var volID int64
	if !cfg.NoVolume {
		existingVol, _, err := client.Volume.GetByName(ctx, volName)
		if err != nil {
			return CreateResult{}, fmt.Errorf("provision: lookup volume: %w", err)
		}
		if existingVol != nil {
			volID = existingVol.ID
		} else {
			volLabels := StandardLabels(clusterLabel, "node-data")
			volLabels["role"] = "data"
			volSize := cfg.VolumeSize
			if volSize == 0 {
				volSize = volumeSizeGB
			}
			volResult, _, err := client.Volume.Create(ctx, hcloudsdk.VolumeCreateOpts{
				Name:     volName,
				Size:     volSize,
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
	}

	return CreateResult{
		ServerID:   serverID,
		ServerIPv4: serverIPv4,
		VolumeID:   volID,
		FirewallID: fwID,
		NetworkID:  networkID,
		PrivateIP:  privateIP,
	}, nil
}

// waveOrder maps each resource type to its deletion wave. Resources in wave 0
// (servers) must be fully deleted before wave 1 (volumes, firewalls) starts,
// because Hetzner rejects deleting resources still attached to a running server.
// Tailscale devices are filtered out before reaching this function (they are
// handled by the provider's step 3 and do not go through the Hetzner sweep).
var waveOrder = map[registry.ResourceType]int{
	registry.ResourceServer:       0,
	registry.ResourceLoadBalancer: 0,
	registry.ResourceVolume:       1,
	registry.ResourceFirewall:     1,
	registry.ResourceNetwork:      2,
	registry.ResourceSSHKey:       2,
	registry.ResourcePrimaryIP:    2,
}

// deletionWaves groups resources into ordered slices so callers can delete each
// wave completely before starting the next.
func deletionWaves(rows []registry.ClusterResource) [][]registry.ClusterResource {
	buckets := map[int][]registry.ClusterResource{}
	maxWave := 0
	for _, r := range rows {
		w := waveOrder[r.ResourceType]
		buckets[w] = append(buckets[w], r)
		if w > maxWave {
			maxWave = w
		}
	}
	waves := make([][]registry.ClusterResource, 0, maxWave+1)
	for i := 0; i <= maxWave; i++ {
		if len(buckets[i]) > 0 {
			waves = append(waves, buckets[i])
		}
	}
	return waves
}
