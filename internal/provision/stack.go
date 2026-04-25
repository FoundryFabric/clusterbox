package provision

import (
	"context"
	"fmt"
	"strconv"

	"github.com/foundryfabric/clusterbox/internal/tailscale"
	"github.com/pulumi/pulumi-hcloud/sdk/go/hcloud"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	serverType   = "cx42"
	volumeSizeGB = 100
	volumeFormat = "ext4"
	volumeLabel  = "data"
)

// ProvisionCluster creates all Hetzner Cloud resources required for one
// clusterbox node:
//
//   - CX42 VM booted from the named snapshot
//   - 100 GB volume (ext4, attached + mounted at /data via cloud-init)
//   - Firewall: 443/tcp + 41641/udp inbound from 0.0.0.0/0; port 22 NOT opened
//   - DNS A record: <ClusterName>.<DNSDomain> → VM public IPv4
//
// Tailscale is activated at first boot using an ephemeral key generated from
// the OAuth credentials in cfg. The auth key value is never written to any log.
func ProvisionCluster(ctx *pulumi.Context, cfg ClusterConfig) error {
	authKey, err := tailscale.GenerateAuthKey(
		context.Background(),
		cfg.TailscaleClientID,
		cfg.TailscaleClientSecret,
	)
	if err != nil {
		return fmt.Errorf("provision: generate tailscale auth key: %w", err)
	}

	userData, err := RenderCloudInit(cfg.ClusterName, authKey)
	if err != nil {
		return err
	}

	return provisionResources(ctx, cfg, userData)
}

// ProvisionStackWithUserData creates all Hetzner Cloud resources exactly like
// ProvisionCluster but accepts a pre-rendered cloud-init user-data string
// instead of calling the Tailscale API. This is the preferred entry point for
// unit tests that use Pulumi's mock framework.
func ProvisionStackWithUserData(ctx *pulumi.Context, cfg ClusterConfig, userData string) error {
	return provisionResources(ctx, cfg, userData)
}

// provisionResources is the internal implementation shared by ProvisionCluster
// and ProvisionStackWithUserData.
//
// POLICY: Every Hetzner Cloud resource created here MUST attach the
// `managed-by=clusterbox` and `cluster-name=<name>` labels (via
// PulumiLabels). The post-operation reconciler in inventory.go relies on
// these labels to track the resource. Resources missing these labels will
// not be tracked and will be flagged as 'unmanaged' on destroy.
func provisionResources(ctx *pulumi.Context, cfg ClusterConfig, userData string) error {
	// --- 1. Firewall ---
	fw, err := hcloud.NewFirewall(ctx, cfg.ClusterName+"-fw", &hcloud.FirewallArgs{
		Name:   pulumi.String(cfg.ClusterName + "-fw"),
		Labels: PulumiLabels(cfg.EffectiveClusterLabel(), "node-firewall"),
		Rules: hcloud.FirewallRuleArray{
			// Allow HTTPS inbound from anywhere (IPv4 + IPv6).
			&hcloud.FirewallRuleArgs{
				Direction: pulumi.String("in"),
				Protocol:  pulumi.String("tcp"),
				Port:      pulumi.StringPtr("443"),
				SourceIps: pulumi.StringArray{
					pulumi.String("0.0.0.0/0"),
					pulumi.String("::/0"),
				},
			},
			// Allow Tailscale WireGuard UDP inbound from anywhere.
			&hcloud.FirewallRuleArgs{
				Direction: pulumi.String("in"),
				Protocol:  pulumi.String("udp"),
				Port:      pulumi.StringPtr("41641"),
				SourceIps: pulumi.StringArray{
					pulumi.String("0.0.0.0/0"),
					pulumi.String("::/0"),
				},
			},
			// Allow ICMP inbound (ping / path-MTU discovery).
			&hcloud.FirewallRuleArgs{
				Direction: pulumi.String("in"),
				Protocol:  pulumi.String("icmp"),
				SourceIps: pulumi.StringArray{
					pulumi.String("0.0.0.0/0"),
					pulumi.String("::/0"),
				},
			},
			// NOTE: port 22 is intentionally NOT listed.
			// SSH is accessible only through Tailscale.
		},
	})
	if err != nil {
		return fmt.Errorf("provision: create firewall: %w", err)
	}

	// --- 2. VM ---
	// Image accepts a snapshot name or ID directly.
	server, err := hcloud.NewServer(ctx, cfg.ClusterName, &hcloud.ServerArgs{
		Name:       pulumi.String(cfg.ClusterName),
		ServerType: pulumi.String(serverType),
		Image:      pulumi.StringPtr(cfg.SnapshotName),
		Location:   pulumi.StringPtr(cfg.Location),
		UserData:   pulumi.StringPtr(userData),
		Labels:     PulumiLabels(cfg.EffectiveClusterLabel(), cfg.ResourceRole),
	})
	if err != nil {
		return fmt.Errorf("provision: create server: %w", err)
	}

	// Hetzner resource IDs are integers at the API level but Pulumi models them
	// as strings. We convert via strconv.Atoi for resources that require IntInput.
	serverIDInt := server.ID().ApplyT(func(id string) (int, error) {
		n, err := strconv.Atoi(id)
		if err != nil {
			return 0, fmt.Errorf("provision: parse server ID %q: %w", id, err)
		}
		return n, nil
	}).(pulumi.IntOutput)

	// Attach the firewall to the server.
	_, err = hcloud.NewFirewallAttachment(ctx, cfg.ClusterName+"-fw-attach", &hcloud.FirewallAttachmentArgs{
		FirewallId: fw.ID().ApplyT(func(id string) (int, error) {
			n, err := strconv.Atoi(id)
			if err != nil {
				return 0, fmt.Errorf("provision: parse firewall ID %q: %w", id, err)
			}
			return n, nil
		}).(pulumi.IntOutput),
		ServerIds: pulumi.IntArray{serverIDInt},
	}, pulumi.DependsOn([]pulumi.Resource{server, fw}))
	if err != nil {
		return fmt.Errorf("provision: attach firewall: %w", err)
	}

	// --- 3. Volume ---
	// volumeLabels combines the standard cluster labels with the legacy
	// "role=data" label that cloud-init's filesystem provisioner relies on.
	volumeLabels := PulumiLabels(cfg.EffectiveClusterLabel(), "node-data")
	volumeLabels["role"] = pulumi.String(volumeLabel)
	vol, err := hcloud.NewVolume(ctx, cfg.ClusterName+"-data", &hcloud.VolumeArgs{
		Name:     pulumi.String(cfg.ClusterName + "-data"),
		Size:     pulumi.Int(volumeSizeGB),
		Location: pulumi.StringPtr(cfg.Location),
		Format:   pulumi.StringPtr(volumeFormat),
		Labels:   volumeLabels,
	})
	if err != nil {
		return fmt.Errorf("provision: create volume: %w", err)
	}

	volIDInt := vol.ID().ApplyT(func(id string) (int, error) {
		n, err := strconv.Atoi(id)
		if err != nil {
			return 0, fmt.Errorf("provision: parse volume ID %q: %w", id, err)
		}
		return n, nil
	}).(pulumi.IntOutput)

	_, err = hcloud.NewVolumeAttachment(ctx, cfg.ClusterName+"-vol-attach", &hcloud.VolumeAttachmentArgs{
		ServerId:  serverIDInt,
		VolumeId:  volIDInt,
		Automount: pulumi.Bool(false), // cloud-init handles the /data mount
	}, pulumi.DependsOn([]pulumi.Resource{server, vol}))
	if err != nil {
		return fmt.Errorf("provision: attach volume: %w", err)
	}

	// --- 4. DNS A record ---
	_, err = hcloud.NewZoneRecord(ctx, cfg.ClusterName+"-dns", &hcloud.ZoneRecordArgs{
		Zone:  pulumi.String(cfg.DNSDomain),
		Name:  pulumi.String(cfg.ClusterName),
		Type:  pulumi.String("A"),
		Value: server.Ipv4Address,
	}, pulumi.DependsOn([]pulumi.Resource{server}))
	if err != nil {
		return fmt.Errorf("provision: create dns record: %w", err)
	}

	// Export useful stack outputs.
	ctx.Export("serverIPv4", server.Ipv4Address)
	ctx.Export("volumeID", vol.ID())
	ctx.Export("firewallID", fw.ID())

	return nil
}
