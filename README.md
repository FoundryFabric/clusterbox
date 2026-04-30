# clusterbox

clusterbox is a Go CLI for provisioning and operating k3s clusters. It supports
Hetzner Cloud, QEMU (local VM), and baremetal targets. A local SQLite registry
tracks every cluster, node, deployment, and cloud resource clusterbox touches.
An embedded addon catalog manages cluster add-ons from a single `addon install`
command.

---

## Table of contents

- [Build from source](#build-from-source)
- [Initial setup](#initial-setup)
- [Provisioning a cluster](#provisioning-a-cluster)
- [Cluster lifecycle](#cluster-lifecycle)
- [Addon system](#addon-system)
- [GitHub Actions runner management](#github-actions-runner-management)
- [Deploying services](#deploying-services)
- [Registry and observability](#registry-and-observability)
- [Context management](#context-management)
- [Secrets and 1Password layout](#secrets-and-1password-layout)
- [Development](#development)

---

## Build from source

Go 1.24 or later is required. The embedded addon catalog and the
`clusterboxnode` agent bundle are compiled in at build time.

```bash
git clone https://github.com/foundryfabric/clusterbox.git
cd clusterbox
make build          # produces ./bin/clusterbox
./bin/clusterbox --version
```

`make build` cross-compiles `clusterboxnode` for `linux/amd64` and
`linux/arm64` and embeds the binaries before linking the CLI. A plain
`go build .` will fail if the agent bundles are missing; use `make build` or
`make agents` first.

---

## Initial setup

### Configure a context

clusterbox stores named contexts in `~/.clusterbox/config.yaml`. Each context
holds 1Password references to the infrastructure credentials it needs — no
secrets touch disk.

```bash
clusterbox login \
  --context foundryfabric \
  --hetzner "op://FoundryFabric/Hetzner/credential" \
  --tailscale-client-id "op://FoundryFabric/Tailscale/client-id" \
  --tailscale-client-secret "op://FoundryFabric/Tailscale/client-secret" \
  --cluster my-cluster
```

`--activate` is on by default, so the new context becomes the active one
immediately.

| Flag | Description |
|---|---|
| `--context` | Name for this context (default: `default`) |
| `--hetzner` | 1Password path for the Hetzner API token |
| `--tailscale-client-id` | 1Password path for the Tailscale OAuth client ID |
| `--tailscale-client-secret` | 1Password path for the Tailscale OAuth client secret |
| `--cluster` | Default cluster name used when `--cluster` is omitted elsewhere |
| `--activate` | Set as current context after saving (default: true) |

### Switch or inspect contexts

```bash
clusterbox context           # show active context
clusterbox context list      # list all configured contexts
clusterbox use-context prod  # switch active context
```

---

## Provisioning a cluster

### Hetzner (default)

```bash
clusterbox up my-cluster --env prod
```

Defaults: `--provider hetzner`, `--region ash`, `--server-type cpx21`,
`--nodes 1`, k3s `v1.32.3+k3s1`.

The command:
1. Provisions a Hetzner server, firewall, private network, and optionally a
   data volume via the hcloud API.
2. Bootstraps k3s over Tailscale SSH.
3. Merges the kubeconfig into `~/.kube/config` and sets `current-context`.
4. Records the cluster and nodes in the local registry.
5. Installs default addons for the provider (see [Addon system](#addon-system)).

**Common flags:**

| Flag | Default | Description |
|---|---|---|
| `--provider` | `hetzner` | Infrastructure provider (`hetzner`, `qemu`, `baremetal`, `k3d`) |
| `--region` | `ash` | Hetzner datacenter (`ash`, `nbg1`, `fsn1`, `hel1`) |
| `--env` | _(required for cloud)_ | Environment label, e.g. `prod`, `staging` |
| `--nodes` | `1` | Total nodes (1 = control-plane only; >1 adds workers) |
| `--server-type` | `cpx21` | Hetzner server type |
| `--k3s-version` | `v1.32.3+k3s1` | k3s version to install |
| `--tailscale-tag` | `tag:server` | Tailscale ACL tag for provisioned devices |
| `--no-volume` | `true` | Skip the separate data volume |
| `--volume-size` | `100` | Data volume size in GB (when `--no-volume=false`) |
| `--no-public-ip` | `false` | Disable public IPv4/IPv6 (requires a NAT gateway) |
| `--skip-addon` | _(none)_ | Skip a default addon; repeatable |
| `--cluster` | `<provider>-<region>` | Override the cluster name |

**Examples:**

```bash
# Single node, no load balancer, no CCM
clusterbox up my-cluster --env prod \
  --skip-addon hcloud-ccm \
  --skip-addon hcloud-csi

# Three-node cluster in Nuremberg
clusterbox up prod-cluster --env prod --region nbg1 --nodes 3

# Skip Traefik (bring your own ingress)
clusterbox up my-cluster --env prod --skip-addon traefik
```

### QEMU (local VM)

```bash
clusterbox up --provider qemu
```

Provisions local QEMU VMs. No cloud credentials needed. The cluster is named
`local` by default. Installs Traefik automatically.

### Baremetal

```bash
clusterbox up --provider baremetal \
  --host 192.168.1.10 \
  --user ubuntu \
  --ssh-key ~/.ssh/id_ed25519
```

Runs `clusterboxnode` on an existing SSH-accessible machine. Only single-node
is supported; add workers with `add-node` after initial provisioning. Installs
Traefik automatically.

| Flag | Description |
|---|---|
| `--host` | Host (and optional port) of the target machine |
| `--user` | SSH user |
| `--ssh-key` | Path to SSH private key |
| `--config` | Optional path to a clusterboxnode YAML config file |

---

## Cluster lifecycle

### Add worker nodes

```bash
clusterbox add-node --cluster my-cluster --count 2
```

Provisions Hetzner VMs and joins them to the existing cluster. `--count`
adds multiple workers in parallel.

| Flag | Default | Description |
|---|---|---|
| `--cluster` | _(required)_ | Cluster to add nodes to |
| `--count` | `1` | Number of nodes to add |
| `--region` | `ash` | Hetzner region |
| `--k3s-version` | `v1.32.3+k3s1` | k3s version |

### Remove worker nodes

```bash
clusterbox remove-node --cluster my-cluster --node worker-1 --node worker-2
```

Drains and deletes each node from the cluster, then destroys the underlying
Hetzner VM. Multiple `--node` flags run in parallel.

### List clusters

```bash
clusterbox list          # table output
clusterbox list --json   # machine-readable JSON
```

### Cluster status

```bash
clusterbox status my-cluster
clusterbox status my-cluster --json
```

Shows nodes, deployments, and cluster metadata from the local registry.

### Destroy a cluster

```bash
clusterbox destroy my-cluster
clusterbox destroy my-cluster --yes       # skip confirmation
clusterbox destroy my-cluster --dry-run   # preview only
```

Tears down all Hetzner resources the cluster owns (servers, volumes, firewalls,
primary IPs, SSH keys, Tailscale device entries). The local registry row is
removed after a successful destroy.

| Flag | Description |
|---|---|
| `--yes` / `-y` | Skip the interactive confirmation prompt |
| `--dry-run` | Print the plan without executing anything |
| `--keep-snapshots` | Preserve any Hetzner snapshots |

### Kubeconfig

Each cluster's kubeconfig is written to `~/.kube/<cluster-name>.yaml` and
merged into `~/.kube/config` automatically on `up`. To retrieve the path:

```bash
clusterbox status my-cluster   # shows KubeconfigPath in the header
```

---

## Addon system

Addons are cluster extensions defined as YAML manifests in `addons/` and
compiled into the binary. Each addon declares required secrets, an install
strategy (`manifests` or `helmchart`), and a role that determines install order.

**Install order by role:** `cloud-controller` → `csi-driver` →
`certificate-manager` → `ingress` → `dns`

### Provider defaults

When `clusterbox up` completes, default addons for the provider install
automatically in role order:

| Provider | Default addons |
|---|---|
| `hetzner` | `hcloud-ccm`, `hcloud-csi`, `traefik` |
| `qemu` | `traefik` |
| `baremetal` | `traefik` |
| `k3d` | _(none)_ |

Skip a default addon at provision time:

```bash
clusterbox up my-cluster --env prod --skip-addon hcloud-ccm --skip-addon hcloud-csi
```

Install it later with `addon install`.

### Addon commands

```bash
# List available addons in the catalog
clusterbox addon list

# List addons installed on a specific cluster
clusterbox addon list --cluster my-cluster

# Install an addon
clusterbox addon install <name> --cluster my-cluster

# Uninstall an addon
clusterbox addon uninstall <name> --cluster my-cluster

# Upgrade an addon to the catalog version
clusterbox addon upgrade <name> --cluster my-cluster
```

`addon install` flags:

| Flag | Description |
|---|---|
| `--cluster` | Target cluster (default: active context cluster) |
| `--mode` | Install mode for multi-mode addons (e.g. `file`, `full`) |

`addon uninstall` flags:

| Flag | Description |
|---|---|
| `--cluster` | Target cluster |
| `--yes` | Skip confirmation prompt |

### Available addons

| Name | Version | Role | Description |
|---|---|---|---|
| `hcloud-ccm` | v1.21.0 | cloud-controller | Hetzner Cloud Controller Manager. Manages node lifecycle and provisions Hetzner Load Balancers for `LoadBalancer` services. Requires `HETZNER_API_TOKEN` and `HETZNER_NETWORK` secrets. |
| `hcloud-csi` | 2.9.0 | csi-driver | Hetzner Block Storage CSI driver. Provisions Hetzner volumes as Kubernetes PersistentVolumes via the `hcloud-volumes` StorageClass. |
| `traefik` | 33.2.1 | ingress | Traefik v3 ingress controller with HTTP→HTTPS redirect. Deployed as a `LoadBalancer` service; the cloud controller provisions one Hetzner LB for all ingress traffic. |
| `cert-manager` | v1.16.3 | certificate-manager | Issues and renews TLS certificates from Let's Encrypt. Installs CRDs and the controller. Use `ClusterIssuer` resources to configure issuers after installation. Not in any provider defaults — opt-in only. |
| `external-dns-cloudflare` | 1.15.0 | dns | Watches `Ingress` and `LoadBalancer` resources and creates DNS records in a Cloudflare zone. Requires `CLOUDFLARE_API_TOKEN`, `EXTERNAL_DNS_DOMAIN_FILTER`, and `EXTERNAL_DNS_OWNER_ID` secrets. Not in any provider defaults — opt-in only. |
| `gha-runner-scale-set` | 0.10.1 | — | GitHub Actions Runner Controller (ARC) in scale-set mode. Installs the controller only; runner scale sets are added via `clusterbox runner add`. |
| `telemetry` | 0.1.0 | — | Observability stack. `--mode file` writes OTLP data to a local PVC. `--mode full` deploys ClickHouse + Grafana. |

### Addon secrets

Addons that require secrets resolve them from the active secrets backend
(1Password by default) at install time. Secrets are looked up by key under the
cluster's provider/region item. See [Secrets and 1Password layout](#secrets-and-1password-layout).

---

## GitHub Actions runner management

Self-hosted GitHub Actions runners run as ARC `AutoscalingRunnerSet` resources
on the cluster. The `gha-runner-scale-set` addon must be installed first.

### Install the controller

```bash
clusterbox addon install gha-runner-scale-set --cluster my-cluster
```

Store GitHub credentials in 1Password before installing (see
[Secrets and 1Password layout](#secrets-and-1password-layout)). The addon
accepts either a GitHub App (recommended) or a PAT.

### Add a runner scale set

```bash
# Repo-scoped runner
clusterbox runner add clusterbox-runners \
  --repo FoundryFabric/clusterbox \
  --cluster my-cluster

# Org-scoped runner with custom concurrency
clusterbox runner add org-runners \
  --repo FoundryFabric \
  --min 1 --max 8 \
  --cluster my-cluster
```

Deploys an `AutoscalingRunnerSet` to the `arc-runners` namespace. The success
message prints the `runs-on:` label to use in workflow files.

| Flag | Default | Description |
|---|---|---|
| `--repo` | _(required)_ | GitHub org/repo slug or full URL |
| `--cluster` | active context | Target cluster |
| `--min` | `0` | Minimum idle runners |
| `--max` | `4` | Maximum concurrent runners |
| `--image` | `ghcr.io/actions/actions-runner:2.323.0` | Runner container image |

### List runner scale sets

```bash
clusterbox runner list --cluster my-cluster
```

### Remove a runner scale set

```bash
clusterbox runner remove clusterbox-runners --cluster my-cluster
```

### Use the runner in a workflow

```yaml
jobs:
  test:
    runs-on: clusterbox-runners   # matches the scale set name
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: make test
```

---

## Deploying services

```bash
clusterbox deploy <service> <version> --cluster my-cluster
clusterbox deploy my-api v1.2.3 --cluster my-cluster --env prod
```

Fetches the release manifest for `<service>@<version>` from GitHub, resolves
secrets, applies via kubectl, and records the deployment in the registry.

| Flag | Default | Description |
|---|---|---|
| `--cluster` | _(required)_ | Target cluster |
| `--env` | `prod` | Target environment (`dev` or `prod`) |

---

## Registry and observability

The local registry is a SQLite file that tracks clusters, nodes, deployments,
and Hetzner resources. It is the source of truth for `list`, `status`,
`history`, `diff`, and `sync`.

### History

```bash
clusterbox history
clusterbox history --cluster my-cluster --service my-api
clusterbox history --limit 20 --json
```

Prints deployment attempts (most recent first). Filters: `--cluster`,
`--service`, `--limit`.

### Diff

```bash
clusterbox diff my-cluster
clusterbox diff my-cluster --json
```

Compares the local registry against live state (kubectl + Pulumi). Exit code 0
means no drift; 1 means drift detected; 2 means error. Use this before `sync`
to preview changes.

### Sync

```bash
clusterbox sync
clusterbox sync my-cluster
clusterbox sync my-cluster --prune --dry-run
```

Reconciles the local registry against live state. By default, drift is reported
but the registry is not modified.

| Flag | Description |
|---|---|
| `--prune` | Delete registry rows whose source-of-truth has disappeared |
| `--dry-run` | Compute the diff without writing to the registry |

### Dashboard

```bash
clusterbox dashboard
clusterbox dashboard --addr 127.0.0.1:9090
clusterbox dashboard --no-browser
```

Starts an embedded read-only HTTP UI at `http://127.0.0.1:7777` (default) and
opens a browser tab automatically.

---

## Context management

```bash
# Show active context
clusterbox context

# List all contexts
clusterbox context list

# Switch active context
clusterbox use-context prod

# Override context for a single command
clusterbox addon list --context staging
```

The global `--context` flag overrides the active context for any command.

---

## Secrets and 1Password layout

clusterbox resolves secrets from 1Password at runtime using the 1Password CLI
(`op`). Credentials are stored as fields inside a single item per
provider+region, named `<provider>-<region>` (e.g. `hetzner-ash`), in the
vault configured by `OP_VAULT`.

### Infrastructure secrets (set via `clusterbox login`)

| 1Password field | Used by |
|---|---|
| `hetzner` | Hetzner API token — all Hetzner operations |
| `tailscale_client_id` | Tailscale OAuth client ID |
| `tailscale_client_secret` | Tailscale OAuth client secret |
| `ghcr_token` | GitHub Container Registry pull token |
| `ghcr_user` | GitHub Container Registry username |

### Addon secrets

Addon secrets are additional fields in the same `<provider>-<region>` item.
Add them before running `addon install`:

| Addon | Required secret keys |
|---|---|
| `hcloud-ccm` | `HETZNER_API_TOKEN`, `HETZNER_NETWORK` |
| `external-dns-cloudflare` | `CLOUDFLARE_API_TOKEN`, `EXTERNAL_DNS_DOMAIN_FILTER`, `EXTERNAL_DNS_OWNER_ID` |
| `gha-runner-scale-set` | `GH_PAT_TOKEN` **or** `GH_APP_ID` + `GH_APP_INSTALLATION_ID` + `GH_APP_PRIVATE_KEY` |
| `telemetry` (full mode) | `GRAFANA_ADMIN_PASSWORD`, `CLICKHOUSE_ADMIN_PASSWORD` |

`EXTERNAL_DNS_OWNER_ID` must be unique per cluster that shares a Cloudflare
zone (use the cluster name).

### Environment variable fallback

For CI or environments without 1Password, export secrets directly:

```bash
export HETZNER_API_TOKEN=...
export TAILSCALE_OAUTH_CLIENT_ID=...
export TAILSCALE_OAUTH_CLIENT_SECRET=...
export GHCR_TOKEN=...
export GHCR_USER=...
```

---

## Development

### Build

```bash
make build       # build clusterbox + cross-compile embedded clusterboxnode
make agents      # rebuild only the embedded clusterboxnode binaries
```

### Test

```bash
make test        # go test ./...
```

### Lint and format

```bash
make lint        # golangci-lint run ./... (golangci-lint must be on PATH)
make fmt         # go fmt ./...
make fmt-check   # verify formatting without modifying files
```

### Release builds

```bash
make rel         # cross-compile clusterbox and clusterboxnode for linux/darwin amd64/arm64 into ./dist/
```

### Repo layout

```
main.go                    entrypoint
cmd/                       cobra commands (one file per top-level command)
internal/
  addon/                   catalog loader and installer
  agentbundle/             embedded clusterboxnode binaries
  apply/                   kubectl-apply helpers
  bootstrap/               k3s bootstrap
  config/                  context config (~/.clusterbox/config.yaml)
  dashboard/               embedded HTTP UI
  node/                    clusterboxnode logic (k3s, hardening)
  provision/               provider interface + Hetzner, QEMU, baremetal, k3d implementations
  registry/                SQLite registry (schema, migrations, sync)
  secrets/                 pluggable secrets backends (1Password, env)
addons/                    addon definitions embedded into the binary
cmd/clusterboxnode/        the remote agent binary installed on cluster nodes
packer/                    base-snapshot Packer template
deploy/                    per-cluster Jsonnet config
docs/                      extended documentation
```

---

## License

Not yet decided. A `LICENSE` file will be added before any external
distribution.
