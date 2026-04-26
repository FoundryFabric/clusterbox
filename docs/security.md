# Security model

This page describes what clusterbox creates in Hetzner Cloud, what the default firewall posture looks like, how SSH and Tailscale fit together, and what `clusterbox destroy` revokes vs. preserves. It is meant to be useful both for an operator deciding whether to trust clusterbox and for one debugging a real issue.

## The tracking policy

clusterbox tracks every Hetzner Cloud resource it provisions. The mechanism is two-part:

1. **Mandatory labels.** Every Pulumi-created Hetzner resource is tagged at creation with:
   - `managed-by=clusterbox`
   - `cluster-name=<name>`
   - `resource-role=<role>` (e.g. `control-plane`, `worker`, `node-firewall`, `ingress-lb`, `ssh-bootstrap`)
2. **Reconciler.** After every cluster-mutating operation (`up`, `add-node`, `remove-node`, `destroy`), clusterbox lists Hetzner resources matching those labels and reconciles them into the local `hetzner_resources` registry table. New resources are recorded; resources that disappeared are marked destroyed.

The label schema is defined in `internal/provision/labels.go`. The reconciler lives in `internal/provision/inventory.go`.

You can verify the labels yourself:

```sh
hcloud server list -l 'managed-by=clusterbox'
hcloud server list -l 'managed-by=clusterbox,cluster-name=prod-ash'
```

Resources missing the labels (created by another tool or by hand) are flagged as **unmanaged** by `clusterbox destroy` and are never touched.

## What clusterbox provisions in Hetzner

The resource types tracked in the inventory (from `internal/registry/types.go`):

| Type                | When created                                | Cleaned up by                                  |
| ------------------- | ------------------------------------------- | ---------------------------------------------- |
| `server`            | `clusterbox up`, `clusterbox add-node`      | `clusterbox destroy` / `remove-node`           |
| `firewall`          | `clusterbox up`                             | `clusterbox destroy`                           |
| `ssh_key`           | `clusterbox up` (uploaded for provisioning) | `clusterbox destroy`                           |
| `network`           | reserved (private network, opt-in)          | `clusterbox destroy`                           |
| `primary_ip`        | reserved (explicit primary IP attachment)   | `clusterbox destroy`                           |
| `volume`            | reserved (additional persistent volumes)    | `clusterbox destroy`                           |
| `load_balancer`     | epic #95 (multi-node clusters)              | `clusterbox destroy`                           |
| `tailscale_device`  | activated at first boot via cloud-init      | `clusterbox remove-node` / `clusterbox destroy` (best-effort revoke) |

`load_balancer` and DNS records arrive in Cluster lifecycle v2 (epic #95). The current shipped surface is everything else.

## Default firewall policy

The firewall provisioned by `clusterbox up` (see `internal/provision/stack.go`) opens these ports inbound:

| Direction | Protocol | Port      | Source                    | Reason                         |
| --------- | -------- | --------- | ------------------------- | ------------------------------ |
| in        | tcp      | 443       | `0.0.0.0/0`, `::/0`       | HTTPS to Traefik on the node   |
| in        | udp      | 41641     | `0.0.0.0/0`, `::/0`       | Tailscale WireGuard            |
| in        | icmp     | (any)     | `0.0.0.0/0`, `::/0`       | Ping / path-MTU discovery      |

**Port 22 is NOT opened from the public internet.** SSH is reachable only through the Tailscale tailnet; this is enforced by the test in `internal/provision/stack_test.go::TestProvisionStack_FirewallRules`.

When the LB lands (epic #95), the per-node firewall rule that exposes 443 to the public internet will narrow to the LB's source range. Until then, every node is directly reachable on 443.

## SSH keys

There are two SSH layers, and they serve different purposes:

1. **Hetzner SSH key** (`hcloud.SshKey`). The operator's public key is uploaded once to Hetzner Cloud so that newly-provisioned servers can have it injected at boot via cloud-init. This is recorded in `hetzner_resources` with `resource_type=ssh_key` and `resource-role=ssh-bootstrap`.
2. **OS-level user.** The packer-built base image creates an unprivileged `clusterbox` user and accepts the key from layer 1 in its `~/.ssh/authorized_keys`. SSH itself is locked down by the hardening script in `packer/scripts/harden.sh` (no root SSH, key-only auth, etc.).

**Rotation.** To rotate the operator key:

1. Add the new key to Hetzner via the operator console or the hcloud CLI.
2. Roll any existing nodes (or accept that the new key only takes effect for new nodes).
3. Remove the old `hcloud.SshKey` row.
4. `clusterbox sync <cluster>` will reconcile the inventory.

There is no automated rotation today.

## Tailscale

Tailscale is the only path to SSH access for clusterbox-managed nodes.

- **Auth key.** Read from the existing secrets backend at the path `<app>/<env>/<provider>/<region>/TAILSCALE_AUTH_KEY` (see `docs/secrets.md` for the resolution mechanics). Use a reusable, ephemeral, pre-approved auth key so the device joins automatically without operator interaction.
- **Activation.** Happens at first boot of each VM via cloud-init. clusterbox does not run `tailscale up` itself.
- **Device registration.** Each node appears as a separate device on the tailnet, named after the cluster + hostname.
- **Revocation.** `clusterbox remove-node` and `clusterbox destroy` issue best-effort device-revocation calls against the Tailscale API. The corresponding `hetzner_resources` row with `resource_type=tailscale_device` is marked destroyed regardless of whether the revoke API call succeeded — operators with strict revocation requirements should periodically audit the tailnet.

**Failure modes:**

- **Auth key expired or exhausted.** The VM boots but Tailscale never registers. Symptom: SSH via Tailscale hostname fails. Fix: rotate the key in the secrets backend.
- **Device entry already revoked** (operator did it manually). The revocation API call returns an error; clusterbox logs a warning and continues.

## Public IPs

Every server provisioned today gets a public IPv4 + IPv6. This is required because the firewall opens 443 directly on each node and Tailscale's WireGuard handshake needs UDP 41641 reachable.

**Future direction.** Once the LB lands (epic #95), worker nodes can plausibly drop their public IPs entirely (LB → private network → workers). That is a deliberate change to the security posture and is **not** the default today. Currently treat every clusterbox node as internet-reachable on 443/tcp.

## The destroy flow

`clusterbox destroy <cluster> [--yes] [--keep-snapshots] [--dry-run]` does the following, in order:

1. Reads the cluster row + active resources from the registry. Prints a summary and prompts for confirmation unless `--yes`.
2. Runs `pulumi destroy` on the cluster's stack(s).
3. Runs the inventory reconciler: lists Hetzner resources still labelled `managed-by=clusterbox,cluster-name=<name>` and ensures every one is gone or directly deletes it via the hcloud SDK.
4. Best-effort revokes Tailscale device entries.
5. Marks the cluster row destroyed via `MarkClusterDestroyed` (soft delete; the row is preserved for audit history).

What `destroy` does NOT do by default:

- **DNS records** are not removed. Operators may want them preserved as historical reference. Manage them yourself if you want them gone.
- **Hetzner snapshots** built by `packer/` are not deleted unless `--keep-snapshots` is omitted (default behavior is to keep them; check the flag wording before relying on it).
- **Anything labelled `managed-by=clusterbox` that doesn't match the cluster name** — wrong-cluster blast radius is intentionally absent.

After destroy, verify:

```sh
hcloud server list -l 'managed-by=clusterbox,cluster-name=<name>'    # expect: empty
hcloud load-balancer list -l 'managed-by=clusterbox,cluster-name=<name>'  # expect: empty (once epic #95 lands)
```

## Threat model boundaries

clusterbox solves a narrow operational problem: provisioning, recording, and tearing down k3s clusters on Hetzner with predictable security defaults. It explicitly does NOT address:

- **Secret rotation in running workloads.** Once a secret is materialised into a k8s `Secret`, clusterbox doesn't track or rotate it. That's the application's job (or cert-manager's, when the cert-manager addon ships in epic #95).
- **In-cluster network policy enforcement.** Flannel (k3s default) doesn't enforce `NetworkPolicy`. Add Calico or Cilium if you need namespace isolation; clusterbox doesn't ship that today.
- **Runtime image scanning.** No clusterbox layer scans container images. Use the registry of your choice's scanning features (or Trivy / Grype) at build time.
- **k3s component CVE patching.** clusterbox installs k3s at a pinned version via k3sup. Upgrades are an operator action; clusterbox doesn't auto-upgrade.
- **Compliance attestation.** No SOC2 / ISO claims. The operator decides whether the documented posture meets their requirements.

When in doubt: read the actual code in `cmd/up.go`, `internal/provision/stack.go`, and `internal/provision/labels.go`. Those are the source of truth; this document is a description of them.

## See also

- [Operator user guide](USER_GUIDE.md)
- [Secrets backends](secrets.md)
- [Registry & local cache](registry.md)
- [Packer template / base image](packer.md)
- [Addons](addons.md)
