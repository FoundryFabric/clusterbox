# clusterbox

clusterbox is a small Go CLI that provisions and operates k3s clusters on
Hetzner Cloud using Pulumi for infrastructure, k3sup for the cluster
bootstrap, and kubectl for everything that follows. It keeps a local SQLite
registry of every cluster, node, deployment, and Hetzner resource it
touches, ships an embedded read-only HTTP dashboard, and supports a small
catalog of cluster addons compiled into the binary.

Day-to-day operator surface:

- `clusterbox up <name>` — provision a new k3s cluster on Hetzner.
- `clusterbox list` / `status <cluster>` — see what's running where.
- `clusterbox dashboard` — open the embedded web UI at `http://127.0.0.1:7777`.
- `clusterbox addon install <name> --cluster <c>` — install an addon onto a cluster.
- `clusterbox destroy <cluster>` — tear down every Hetzner resource clusterbox
  provisioned for that cluster.

clusterbox tracks every Hetzner-side resource it provisions (servers, volumes,
firewalls, primary IPs, SSH keys, Tailscale device entries) in its inventory
table so `destroy` can sweep them all without leaving stragglers.

---

## Quickstart

### Build from source

clusterbox is shipped as source today; there is no published binary. Build it
yourself:

```bash
git clone https://github.com/foundryfabric/clusterbox.git
cd clusterbox
go build -o clusterbox .
./clusterbox --help
```

Go 1.24+ is required (see `go.mod`). The build is hermetic — addons live under
`addons/` and are embedded into the binary at compile time via `//go:embed`.

### Configure credentials

```bash
export HETZNER_API_TOKEN=...           # provisioning + destroy
export TAILSCALE_OAUTH_CLIENT_ID=...   # tailnet auth-key generation
export TAILSCALE_OAUTH_CLIENT_SECRET=...
export PULUMI_ACCESS_TOKEN=...         # state backend
export GHCR_USER=...                   # imagePullSecret for ghcr.io
export GHCR_TOKEN=...

# Choose a secrets backend (default: dev). See docs/secrets.md.
export SECRETS_BACKEND=dev
```

### Provision a cluster

```bash
./clusterbox up my-first-cluster
```

That single command runs Pulumi (VM + firewall + DNS), bootstraps k3s over
Tailscale SSH via k3sup, creates the `ghcr.io` imagePullSecret, applies the
base manifests, and writes the new cluster + nodes into the local registry.

### See what's running

```bash
./clusterbox list                 # every cluster
./clusterbox status my-first-cluster
./clusterbox dashboard            # embedded web UI on http://127.0.0.1:7777
```

### Install an addon

```bash
./clusterbox addon list
./clusterbox addon install gha-runner-scale-set --cluster my-first-cluster
```

### Tear it down

```bash
./clusterbox destroy my-first-cluster
```

`destroy` runs Pulumi destroy, reconciles the local Hetzner inventory, and
sweeps any stragglers — there are no lingering servers, volumes, or
firewalls when it returns. DNS records are not auto-removed.

---

## Documentation

- [`docs/USER_GUIDE.md`](docs/USER_GUIDE.md) — long-form operator tour, top to
  bottom.
- [`docs/registry.md`](docs/registry.md) — schema, sync semantics, dashboard.
- [`docs/secrets.md`](docs/secrets.md) — secret resolution backends and
  configuration (`dev`, `onepassword`, `vault`).
- [`docs/packer.md`](docs/packer.md) — the hardened base snapshot.
- [`docs/addons.md`](docs/addons.md) — what addons are, how to install them,
  how to author one.
- [`docs/security.md`](docs/security.md) — security model: tracking policy,
  default firewall, SSH-via-Tailscale, what `destroy` revokes vs. preserves.

---

## Repo layout

```
main.go              entrypoint (calls cmd.Execute()).
cmd/                 cobra commands (one file per top-level command).
internal/            non-public Go packages.
  addon/             catalog loader + installer for addons embedded into the binary.
  apply/             kubectl-apply helpers used by `clusterbox up`.
  bootstrap/         k3sup invocation.
  dashboard/         embedded HTTP UI (templates + CSS via //go:embed).
  provision/         Pulumi program for Hetzner-side resources.
  registry/          local SQLite cache (factory + sqlite/ + sync/ + migrations).
  secrets/           pluggable secrets backends (dev, onepassword, vault).
  tailscale/         tailnet auth-key generation.
addons/              addon directories embedded into the binary.
manifests/           base manifests applied to every new cluster.
packer/              base-snapshot Packer template (see docs/packer.md).
deploy/              per-cluster Jsonnet config and dev secrets file.
docs/                user-facing documentation.
```

---

## Build, test, lint

```bash
go build -o clusterbox .                          # produce the binary
go test ./...                                     # unit tests
go test -tags integration ./...                   # opt-in integration tests
go fmt ./...                                      # format
go vet ./...                                      # static analysis
golangci-lint run ./...                           # full linter (if installed)
```

Integration tests live under each package's `*_test.go` files behind the
`integration` build tag. They use real on-disk SQLite registries and fakes
for kubectl / secrets — no network is required.

---

## License

Not yet decided. The repo will gain a `LICENSE` file before any external
distribution.
