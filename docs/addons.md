# Addons

clusterbox ships a small catalog of cluster addons compiled into the binary.
This document covers what an addon is, how the manifest format works, the
CLI surface, the install/uninstall flow, and how to add a new one.

---

## What an addon is

An **addon** is a discrete, opt-in piece of cluster-side functionality —
typically a controller, an operator, or a sidecar injector — that
clusterbox knows how to install onto a cluster as a single unit and track
in the registry.

What an addon is **not**:

- It is not a regular service deploy. Service deploys go through
  `clusterbox deploy <service> <version>` and are owned by service repos.
  Addons live *with* the clusterbox binary.
- It is not a generic Helm-chart wrapper. The catalog is a curated list
  baked into a release; you add to it by sending a PR, not by pointing the
  CLI at an arbitrary chart.
- It is not a substitute for GitOps. Addons are pull-based on the operator's
  workstation, not driven by a controller in-cluster.

Each addon's outcome is recorded in the registry as a row in the
`deployments` table with `kind='addon'` (alongside service deploys with
`kind='app'`). See [`docs/registry.md`](registry.md) for the schema.

---

## Where addons live

Every addon is a directory under `addons/` at the root of this repo,
embedded into the binary at build time via `//go:embed`:

```
addons/
  <name>/
    addon.yaml       # required manifest, parsed into internal/addon.Addon
    manifests/       # required directory; raw YAML or a single helmchart.yaml
      *.yaml
    README.md        # required operator-facing description
```

`addons/<name>/manifests/.gitkeep` is preserved by the loader so an addon
with an empty manifest tree (e.g. a stub for a future feature) round-trips
cleanly.

---

## addon.yaml schema

```yaml
name: gha-runner-scale-set                           # must match the directory name
version: 0.10.1                                      # surfaced in `addon list`
description: GitHub Actions Runner Controller ...    # one-line; required
strategy: helmchart                                  # 'manifests' or 'helmchart'
requires: []                                         # other addons that must be installed first
secrets:                                             # keys resolved at install time
  - key: GH_APP_ID
    description: GitHub App ID (recommended over PAT)
    required: false
  - key: GH_APP_INSTALLATION_ID
    description: GitHub App Installation ID
    required: false
  - key: GH_APP_PRIVATE_KEY
    description: GitHub App private key (PEM, multiline)
    required: false
  - key: GH_PAT_TOKEN
    description: GitHub PAT alternative
    required: false
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `name` | string | yes | Must equal the directory name. |
| `version` | string | yes | Free-form; surfaced verbatim in `addon list`. |
| `description` | string | yes | One-line summary. |
| `strategy` | string | yes | `manifests` or `helmchart`. |
| `requires` | list of strings | no | Names of addons that must be installed on the cluster before this one. |
| `secrets[].key` | string | yes (per item) | Secret key, looked up via the configured backend. |
| `secrets[].description` | string | no | Operator-facing hint. |
| `secrets[].required` | bool | no | Required keys missing at install time abort the install (with all missing keys listed in one error). Optional keys silently render as the empty string. |

The loader runs with `KnownFields(true)`, so typos in `addon.yaml` surface
as errors with line numbers rather than being silently ignored.

---

## Manifest strategies

### `strategy: manifests`

Every regular file under `manifests/` is rendered, concatenated with
`---\n` separators, and applied via a single
`kubectl apply -f <tmpfile>` per cluster. Substitution rules:

- The token `${KEY}` is replaced by the resolved secret value when `KEY`
  is present in the resolver's bundle.
- Tokens that don't match the regex `\$\{[A-Z0-9_]+\}` (e.g. `${{ }}`,
  `${chart.version}`) are left in place — Helm/Kustomize templating is
  not accidentally substituted.
- Tokens that *do* match the regex but are absent from the resolved
  bundle are left in place as `${KEY}` so kubectl-apply fails loudly
  rather than silently rendering an empty string.

### `strategy: helmchart`

`manifests/` contains a single `helmchart.yaml` describing a Helm chart to
install. Direct `kubectl apply` of helmchart-strategy addons by the
installer is still landing — until then the gha-runner-scale-set addon
documents a manual workaround (render the manifest by hand and apply it
yourself). See the addon's README.

---

## CLI

### List addons

```bash
clusterbox addon list                           # the catalog (all addons in this binary)
clusterbox addon list --cluster <c>             # what's actually installed on a cluster
clusterbox addon list --json                    # machine-readable
```

Catalog mode reads from the embedded catalog. Installed mode filters
deployments rows where `kind='addon'`.

### Install

```bash
clusterbox addon install <name> --cluster <c>
```

The flow:

1. Look up the addon in the catalog.
2. Look up the cluster in the registry to derive `env`/`provider`/`region`
   for secrets resolution and the kubeconfig path for kubectl.
3. Resolve the secret bundle. Required missing keys abort with a single
   error listing every missing key.
4. Verify every name in `requires` is currently installed
   (`kind='addon'`) on the target cluster.
5. Render the manifest tree with `${KEY}` substitution.
6. Run `kubectl --kubeconfig <path> apply -f <tmpfile>`.
7. Upsert a `deployments` row (`kind='addon'`) and append a `rolled_out`
   history row.

A failure at any step appends a `failed` history row and returns the
original error. The deployments row is never written on a failure path.

### Uninstall

```bash
clusterbox addon uninstall <name> --cluster <c>            # interactive
clusterbox addon uninstall <name> --cluster <c> --yes      # CI mode
```

Uninstall is the inverse:

1. Look up the deployments row to learn the installed version.
2. Re-render the catalog manifests with the same secret substitution.
3. Run `kubectl delete -f --ignore-not-found` so the operation is
   idempotent against partial installs.
4. Delete the deployments row and append a `uninstalled` history entry.

If the catalog version differs from the recorded version, a warning is
printed and the uninstall proceeds with the catalog manifests rather than
refusing — that keeps an operator unblocked when the catalog has rolled
forward.

### Upgrade

```bash
clusterbox addon upgrade <name> --cluster <c>
```

Re-applies the addon's manifests at the current catalog version. Because
`kubectl apply` is idempotent, upgrade is the same code path as install —
the deployments row's `Version` and `DeployedAt` columns advance, and a
fresh history row is appended.

---

## Secrets

Addons use the same secrets backend as `clusterbox deploy`. The lookup
tuple is `(app=<addon-name>, env, provider, region)`; `env`, `provider`,
and `region` are read from the cluster's row in the registry. The exact
storage path depends on the backend (see [`docs/secrets.md`](secrets.md)).

For each addon, the operator stores secrets under that 4-tuple before
running `addon install` — the installer fails fast if a required key is
missing, listing every missing key in a single error.

Secret values never touch disk during install. Resolution returns a map
in memory; the rendered manifest is written to a temp file just long
enough for `kubectl apply` to read it, then removed.

---

## Visibility

Once installed, addons appear alongside service deploys in:

- **`clusterbox status <cluster>`** — addons show up in the deployments
  section. The kind column distinguishes them from service deploys.
- **`clusterbox addon list --cluster <c>`** — installed-mode shows only
  `kind='addon'` rows.
- **`clusterbox history`** — every install / failed / uninstalled attempt
  is recorded as a row in `deployment_history`. Filter with `--service
  <addon-name>`.
- **`clusterbox diff <cluster>`** — addons participate in drift detection
  exactly like service deploys.
- **The dashboard** (`clusterbox dashboard`) — the cluster-detail page
  lists deployments including addons.

Addons that fail to install **do not** create a deployments row, but they
do create a `failed` history row so the audit trail captures the attempt.

---

## Available addons today

| Name | Version | Description | Status |
| --- | --- | --- | --- |
| [`gha-runner-scale-set`](../addons/gha-runner-scale-set/README.md) | 0.10.1 | GitHub Actions Runner Controller (scale-set mode) for self-hosted runners. | helmchart strategy install via clusterbox is still landing — see the addon README. |

cert-manager and related production-baseline addons are tracked under epic
#95; they'll appear in `clusterbox addon list` once shipped.

---

## Authoring a new addon

1. Create `addons/<name>/`.
2. Drop in a valid `addon.yaml` (see schema above).
3. Place either Kubernetes YAML files (for `strategy: manifests`) or a
   single `helmchart.yaml` (for `strategy: helmchart`) under
   `manifests/`. Use `${KEY}` placeholders for any secret values; declare
   each `KEY` in `addon.yaml`'s `secrets:` list.
4. Write a `README.md` covering: what it installs, what credentials it
   needs, how to verify, common failure modes.
5. Re-build clusterbox; the new addon is automatically picked up by the
   embedded catalog. Run `clusterbox addon list` to confirm.

The catalog loader rejects (with a line-numbered error) any of: directory
name not matching `addon.yaml`'s `name`, duplicate names, unrecognised
strategy, secrets entries with an empty `key`, or unknown YAML fields. A
missing `manifests/` directory is also a hard error — addons must ship
the directory even if it only contains `.gitkeep`.

---

## See also

- [`docs/registry.md`](registry.md) — schema for `deployments` and
  `deployment_history` (where addons appear).
- [`docs/secrets.md`](secrets.md) — how the secrets resolver picks up
  per-addon credentials.
- `internal/addon/installer.go` — install / uninstall / upgrade
  implementation, line by line.
