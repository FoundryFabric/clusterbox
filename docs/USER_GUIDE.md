# clusterbox User Guide

A linear walk-through for a new operator: starting from "I have a Hetzner
account and a Tailscale tailnet," ending at "I have a deployed service and
an installed addon on a managed k3s cluster."

If you only need a reference for a single command, run `clusterbox <cmd>
--help`. This guide is the long-form companion.

---

## 1. What clusterbox does

clusterbox provisions and operates k3s clusters on Hetzner Cloud. One CLI
binary owns the lifecycle:

- **Provisioning.** Pulumi creates the Hetzner-side resources (VM, volume,
  firewall, primary IP, DNS record). k3sup installs k3s over Tailscale SSH.
- **Day-to-day operation.** Add or remove worker nodes, deploy services,
  inspect state, reconcile drift.
- **Visibility.** A local SQLite registry caches every cluster, node,
  deployment, and Hetzner resource clusterbox has touched. An embedded
  read-only HTTP dashboard renders the same data.
- **Addons.** A small in-binary catalog of opt-in cluster addons (CI runner
  controllers, etc.) installable per cluster.
- **Teardown.** `clusterbox destroy` runs Pulumi destroy and reconciles the
  Hetzner-side inventory so no resources are left behind.

It deliberately does **not** ship: a generic CD pipeline, a service mesh, a
managed-database layer, or a hosted control plane. clusterbox is a thin
operator-facing CLI; service deploys go through `clusterbox deploy`, and
that's the extent of its app surface.

---

## 2. Prerequisites

You need:

- **A Hetzner Cloud project** with an API token that has read+write access.
  Set as `HETZNER_API_TOKEN`.
- **A Tailscale tailnet** with an OAuth client whose key has the `auth_keys`
  scope. Set as `TAILSCALE_OAUTH_CLIENT_ID` and `TAILSCALE_OAUTH_CLIENT_SECRET`.
- **A Pulumi access token** (cloud or self-hosted) for state. Set as
  `PULUMI_ACCESS_TOKEN`.
- **A `ghcr.io` pull credential** (username + token). Set as `GHCR_USER` and
  `GHCR_TOKEN`. clusterbox creates an imagePullSecret in every new cluster.
- **An SSH key** at `~/.ssh/id_ed25519` (clusterbox uses this to bootstrap
  k3s over Tailscale). The matching public key must be the one baked into the
  base snapshot (see [`docs/packer.md`](packer.md)).
- **A secrets backend.** Default is `dev` (a JSON file in the service repo).
  Production uses 1Password or Vault. See [`docs/secrets.md`](secrets.md).

```bash
export HETZNER_API_TOKEN=...
export TAILSCALE_OAUTH_CLIENT_ID=...
export TAILSCALE_OAUTH_CLIENT_SECRET=...
export PULUMI_ACCESS_TOKEN=...
export GHCR_USER=...
export GHCR_TOKEN=...
export SECRETS_BACKEND=dev   # or onepassword | vault
```

The Hetzner side also needs a base snapshot named `clusterbox-base-v0.1.0`
in the project. Build it with `cd packer && packer build ...` — see
[`docs/packer.md`](packer.md).

---

## 3. Your first cluster

```bash
clusterbox up my-first-cluster
```

This is the full provisioning flow. Expected output:

```
[1/6] Generating Tailscale auth key...
[2/6] Running Pulumi (VM + volume + firewall + DNS)...
[3/6] Tailscale activates at first boot via cloud-init (no action required).
[4/6] Bootstrapping k3s via k3sup over Tailscale SSH...
[5/6] Creating ghcr.io imagePullSecrets...
[6/6] Applying base manifests...
Cluster "my-first-cluster" is up. Kubeconfig: ~/.kube/clusterbox/my-first-cluster.yaml
```

Step by step:

1. clusterbox mints a one-shot Tailscale auth key for the new node.
2. Pulumi creates the VM (from the hardened snapshot), volume, firewall,
   primary IP, and a DNS A record at `<cluster>.foundryfabric.dev`.
3. cloud-init on the VM runs `tailscale up` with the auth key from step 1;
   the node joins the tailnet and becomes reachable by hostname.
4. k3sup installs k3s at the pinned version, writes a kubeconfig to disk.
5. The `ghcr.io` imagePullSecret is created so cluster workloads can pull
   private images.
6. Base manifests under `manifests/` are applied (FDB operator, OTel
   Collector, Traefik, etc.).

After step 6 clusterbox writes the cluster + its control-plane node into the
local registry and runs the Hetzner-side reconciler so every resource it
just created is recorded in the inventory table. Both writes are
best-effort; an upstream success is never masked by a registry failure.

Useful flags:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--provider` | `hetzner` | Infrastructure provider (only `hetzner` today). |
| `--region` | `ash` | Hetzner location. |
| `--nodes` | `1` | Initial node count (1 = control-plane only). |
| `--cluster` | `<provider>-<region>` | Cluster name (auto-derived if not set). |
| `--k3s-version` | pinned default | Override the k3s version installed by k3sup. |

---

## 4. Daily operations

Every command below is one line. Pair it with `--help` for the full surface.

### `clusterbox list`

Show every cluster the registry knows about.

```bash
clusterbox list
```

### `clusterbox status <cluster>`

Show the registry's view of one cluster: header, nodes, current deployments.

```bash
clusterbox status my-first-cluster
```

Add `--json` for machine-readable output.

### `clusterbox add-node --cluster <c>`

Provision a new Hetzner VM and join it to the cluster as a worker.

```bash
clusterbox add-node --cluster my-first-cluster
```

### `clusterbox remove-node --cluster <c> --node <hostname>`

Drain the node, delete it from k3s, and destroy the underlying Hetzner VM.

```bash
clusterbox remove-node --cluster my-first-cluster --node my-first-cluster-2
```

### `clusterbox deploy <service> <version> --cluster <c>`

Resolve secrets, create the `<service>-secrets` Secret, apply the manifest,
and wait for the rollout. Records both `deployments` and a
`deployment_history` row.

```bash
clusterbox deploy myservice v1.2.3 --cluster my-first-cluster
```

The `--env` flag (default `prod`) drives the secrets resolution path:
`<app>/<env>/<provider>/<region>/<key>`.

### `clusterbox history`

Show every deployment attempt — successes and failures — in
reverse-chronological order.

```bash
clusterbox history --cluster my-first-cluster --service myservice --limit 20
```

### `clusterbox sync [<cluster>]`

Reconcile the registry against Pulumi (for nodes) and kubectl (for
deployments). `--prune` deletes registry rows whose source-of-truth has
disappeared; `--dry-run` previews without writing.

```bash
clusterbox sync my-first-cluster
```

### `clusterbox diff <cluster>`

Single-cluster preview of what `sync` would change. Read-only.

```bash
clusterbox diff my-first-cluster
```

### `clusterbox destroy <cluster>`

Tear down the cluster: confirm, run Pulumi destroy, reconcile the Hetzner
inventory, sweep stragglers, and soft-delete the cluster row in the
registry. `--dry-run` prints the plan; `--yes` skips the prompt; DNS records
are not auto-removed.

```bash
clusterbox destroy my-first-cluster --dry-run
clusterbox destroy my-first-cluster
```

### `clusterbox dashboard`

Run an embedded read-only HTTP server (default `127.0.0.1:7777`) that
renders the same data as `list` / `status` / `history`.

```bash
clusterbox dashboard
clusterbox dashboard --addr 0.0.0.0:9000 --no-browser
```

---

## 5. Addons

An **addon** is a chunk of cluster-side functionality (a controller, an
operator, a sidecar injector) that clusterbox installs as a discrete unit
and tracks in the registry as a `kind=addon` deployment row. Addons are
distinct from regular service deploys — they're shipped *with* the
clusterbox binary, not deployed *by* operators authoring service manifests.

### See what's available

```bash
clusterbox addon list                         # the catalog (everything in this binary)
clusterbox addon list --cluster my-first-cluster   # what's installed
```

### Worked example: gha-runner-scale-set

[`addons/gha-runner-scale-set/`](../addons/gha-runner-scale-set/README.md)
installs the GitHub Actions Runner Controller (scale-set mode). It
references four credential keys you store in the configured secrets
backend, then creates a Secret + Helm chart resource on the cluster.

Walk-through:

1. **Create a GitHub App** with the permissions documented in the addon
   README (Actions read+write, Administration read+write, Checks read,
   Metadata read; org-scoped runners also need Self-hosted runners
   read+write). Note the App ID and Installation ID, generate a private
   key.
2. **Store the secrets** in your backend, scoped to
   `app=gha-runner-scale-set` and the env/provider/region of the target
   cluster:
   - `GH_APP_ID`
   - `GH_APP_INSTALLATION_ID`
   - `GH_APP_PRIVATE_KEY`
   - or `GH_PAT_TOKEN` instead of the three GitHub App keys.
3. **Install:**
   ```bash
   clusterbox addon install gha-runner-scale-set --cluster my-first-cluster
   ```
4. **Verify** the controller pod is healthy:
   ```bash
   kubectl --kubeconfig <kubeconfig> get pods -n arc-systems
   ```

The addon installs only the controller. Per-repo or per-org
`AutoscalingRunnerSet` resources are deployed separately by the operator;
the addon's README walks through a sample.

> **Heads up:** the gha-runner-scale-set addon ships with the `helmchart`
> manifest strategy. Direct `kubectl apply` of helmchart strategy addons by
> the installer is still landing — until then the rendered manifests can be
> inspected and applied by hand. See the addon's README for the workaround.

cert-manager and similar production-baseline addons land via epic #95;
they'll appear in `clusterbox addon list` once shipped, and this guide will
gain a worked example for them at that point.

### Uninstall and upgrade

```bash
clusterbox addon uninstall gha-runner-scale-set --cluster my-first-cluster
clusterbox addon upgrade   gha-runner-scale-set --cluster my-first-cluster
```

`uninstall` re-renders the addon's manifests (substituting the same
secrets) and runs `kubectl delete -f --ignore-not-found` so it's idempotent
against partial installs. It then deletes the registry row and appends a
`uninstalled` history entry. `upgrade` is `kubectl apply` against the
current catalog version; the deployments row's Version column advances on
success.

For full reference, see [`docs/addons.md`](addons.md).

---

## 6. The registry file

```
~/.clusterbox/registry.db    (mode 0600 in directory mode 0700)
```

The registry is a **local SQLite cache**, not the source of truth. Pulumi
owns nodes, kubectl owns deployments, Hetzner owns VMs. clusterbox writes
to the registry every time a mutating command succeeds so visibility
commands (`list`, `status`, `history`, `diff`, `dashboard`) don't have to
re-query the network.

If the file is corrupted or you wipe it, `clusterbox sync` rebuilds it from
live state. If a registry write fails after a Pulumi or kubectl operation
succeeded, clusterbox prints `warning: registry write failed: <err>` to
stderr and continues — the upstream operation succeeded, that's the truth
that matters.

For schema, sync semantics, and the dashboard, see
[`docs/registry.md`](registry.md).

---

## 7. Security

clusterbox tracks every Hetzner Cloud resource it provisions via mandatory `managed-by=clusterbox` and `cluster-name=<name>` labels plus a post-operation reconciler. You can audit at any time with:

```sh
hcloud server list -l 'managed-by=clusterbox,cluster-name=<name>'
```

Four operator-relevant points:

- **Default firewall posture.** Nodes open inbound `tcp/443` (Traefik), `udp/41641` (Tailscale WireGuard), and ICMP. **Port 22 is not exposed publicly** — SSH is reachable only through the Tailscale tailnet.
- **SSH-via-Tailscale.** Hetzner injects an `hcloud.SshKey` into new VMs at boot; the operator user (`clusterbox`) accepts that key. Public SSH is blocked at the firewall; you reach nodes by their Tailscale hostname.
- **What `clusterbox destroy` revokes.** Pulumi state, Hetzner-tracked resources (servers, firewalls, ssh keys, networks, volumes, primary IPs), and Tailscale device entries (best-effort).
- **What `clusterbox destroy` preserves.** DNS records (manage them yourself), Hetzner snapshots from the packer flow, and the cluster row in the registry (soft-deleted for audit history).

Full detail in [`docs/security.md`](security.md).

---

## 8. Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `clusterbox up` fails at `[2/6]` with a Pulumi error mentioning the snapshot | The `clusterbox-base-v0.1.0` snapshot doesn't exist in your Hetzner project | Build it: `cd packer && packer build ...` (see `docs/packer.md`). |
| `clusterbox up` hangs at `[4/6] Bootstrapping k3s` | Tailscale ACLs block your workstation from SSH-ing the new node, or the SSH key path is wrong | Verify `~/.ssh/id_ed25519` exists and matches the snapshot's authorized key; check Tailscale ACLs allow your tagged user → tagged node on tcp:22. |
| `clusterbox deploy` fails with "missing required secrets" | Secrets aren't stored under the cluster's `<env>/<provider>/<region>` path in your backend | Check `SECRETS_BACKEND` and the backend-specific paths in `docs/secrets.md`; for `onepassword` confirm the item title is `<env>-<provider>-<region>`. |
| `kubectl ... unable to read kubeconfig` | The kubeconfig path the registry has for this cluster is stale (the file moved) | Re-derive: `clusterbox sync <cluster>`; if that fails, re-run `clusterbox up` with the same cluster name (Pulumi is idempotent). |
| `addon install` errors with `strategy "helmchart" is not yet supported` | helmchart strategy installation is still landing | See the addon's README for the manual `kubectl apply` workaround; tracked as a follow-up. |
| `clusterbox list` shows clusters that no longer exist | Hetzner-side state was changed outside clusterbox | `clusterbox sync --prune` to reconcile and remove orphan rows. |
| `clusterbox destroy` reports stragglers | The Hetzner inventory found resources tagged for this cluster that Pulumi didn't manage | The destroy command sweeps them; if the sweep itself errors, re-run `destroy` (it's idempotent). |
| `clusterbox up` errors with `unsupported registry schema version` | An older binary is reading a database written by a newer one | Either upgrade the binary or delete `~/.clusterbox/registry.db` and re-run `clusterbox sync` (registry is a cache). |

---

## 9. Reference

- [`docs/registry.md`](registry.md) — registry schema and sync internals.
- [`docs/secrets.md`](secrets.md) — secret backends and the path convention.
- [`docs/packer.md`](packer.md) — how the base snapshot is built.
- [`docs/addons.md`](addons.md) — addon system reference.
- [`docs/security.md`](security.md) — security model, firewall, SSH, Tailscale, destroy flow.
- [`addons/gha-runner-scale-set/README.md`](../addons/gha-runner-scale-set/README.md)
  — operator-facing setup for the bundled GitHub Actions runner addon.
