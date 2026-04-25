# Registry

clusterbox keeps a local cache of every cluster, node, and deployment it knows about so the operator can answer "what is currently running where" without re-querying Pulumi or kubectl. This document covers the schema, where the file lives, write semantics, conflict resolution, the new commands, and the embedded dashboard.

---

## How it works

The registry is a local SQLite database written to by every command that mutates infrastructure (`up`, `deploy`, `add-node`, `remove-node`) and read by the visibility commands (`list`, `status`, `history`, `diff`) and the embedded dashboard.

It is a **cache, not the source of truth**. Pulumi owns nodes; Kubernetes owns deployments; Hetzner owns VMs. The registry exists so visibility doesn't require network calls every time.

```
~/.clusterbox/registry.db    (mode 0600 in directory mode 0700)
```

The directory is created on first use. If you delete the file, the next `clusterbox sync` rebuilds it from live state.

---

## Schema

```sql
clusters (
  name           TEXT PRIMARY KEY,
  provider       TEXT NOT NULL,
  region         TEXT NOT NULL,
  env            TEXT NOT NULL,
  created_at     TIMESTAMP NOT NULL,
  kubeconfig_path TEXT NOT NULL,
  last_synced_at TIMESTAMP
);

nodes (
  cluster_name TEXT NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
  hostname     TEXT NOT NULL,
  role         TEXT NOT NULL,             -- 'control-plane' | 'worker'
  joined_at    TIMESTAMP NOT NULL,
  PRIMARY KEY (cluster_name, hostname)
);

deployments (
  cluster_name TEXT NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
  service      TEXT NOT NULL,
  version      TEXT NOT NULL,
  deployed_at  TIMESTAMP NOT NULL,
  deployed_by  TEXT NOT NULL,
  status       TEXT NOT NULL,             -- 'rolled_out' | 'failed' | 'rolling'
  PRIMARY KEY (cluster_name, service)
);

deployment_history (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  cluster_name        TEXT NOT NULL,
  service             TEXT NOT NULL,
  version             TEXT NOT NULL,
  attempted_at        TIMESTAMP NOT NULL,
  status              TEXT NOT NULL,
  rollout_duration_ms INTEGER NOT NULL,
  error               TEXT NOT NULL DEFAULT ''
);

schema_version (version INTEGER NOT NULL);
```

`deployment_history` is append-only: every deploy attempt (success or failure) lands one row. `deployments` is the rolling current state — one row per `(cluster, service)` pair.

---

## Migrations

Migration SQL files live under `internal/registry/migrations/sqlite/` and are embedded at build time via `//go:embed`. Each file is named `<version>_<description>.sql` (e.g. `0001_init.sql`). The migrator runs each file with a higher version than the current `schema_version` row inside its own transaction, then bumps the version.

The directory layout deliberately reserves a sibling `internal/registry/migrations/ff/` slot for a future FoundryFabric-backed registry. Adding that backend later is purely additive — the SQLite path stays where it is.

Migrations are **forward-only**:

- An older binary against a newer database errors loudly with `unsupported registry schema version (db is newer than this binary)`.
- A newer binary against an older database upgrades the schema in place.

To add a migration: drop a new file `0002_<description>.sql` next to the existing one and rebuild. The migrator picks it up automatically.

---

## Write semantics

Registry writes from the four mutating commands are **best-effort**. If a registry write fails after a Pulumi or kubectl operation succeeds, clusterbox prints `warning: registry write failed: <err>` to stderr and continues. The command's exit code reflects the underlying infrastructure operation, not the registry.

The reasoning: if the upstream system succeeded, the operator's intent is achieved. The registry is a cache of that fact. Letting a cache failure mask a real success would be the worst of both worlds. `clusterbox sync` is the recovery path.

---

## Reading the registry

| Command | What it shows |
| --- | --- |
| `clusterbox list` | Every cluster, with node count, service count, last-synced timestamp. |
| `clusterbox status <cluster>` | One cluster's nodes plus current deployments. |
| `clusterbox history [--cluster X] [--service Y] [--limit N]` | Deploy attempts in reverse-chronological order, including failures. |
| `clusterbox diff <cluster>` | Drift between the registry and live state, without writing. |
| `clusterbox sync [<cluster>] [--prune] [--dry-run]` | Reconcile the registry against Pulumi + kubectl. |
| `clusterbox dashboard [--addr <host:port>] [--no-browser]` | Embedded read-only HTTP dashboard. |

All commands accept `--json` (where output is structured) for machine-readable use.

---

## Sync semantics

`clusterbox sync` queries the source-of-truth systems and reconciles the registry to match:

1. **Nodes:** Pulumi enumerates the stacks for the cluster's project; rows are upserted, and rows whose Pulumi stack disappeared are removed unconditionally (nodes are owned by Pulumi).
2. **Deployments:** `kubectl get deployments -A -o json` lists live workloads; each is matched into the registry's `(cluster, service)` keyspace. Rollout status maps to `rolled_out`, `rolling`, or `failed`.
3. **Drift:**
   - Cluster present in registry but no Pulumi stack → warning. Retain the row unless `--prune`.
   - Service in registry but not in kubectl → warning. Retain the row unless `--prune`.
   - Service in kubectl but not in registry → insert (handles deploys made outside clusterbox).
4. `last_synced_at` is updated only on a fully-successful per-cluster reconcile.

`--dry-run` computes the diff and prints the summary without writing to the registry. `clusterbox diff <cluster>` is the dedicated single-cluster preview command and shares the same comparison logic.

---

## Embedded dashboard

`clusterbox dashboard` runs an HTTP server (default `127.0.0.1:7777`) that serves a small read-only UI from the same Go binary. Templates and CSS are embedded via `//go:embed`; there is no separate frontend project, no JavaScript framework, no npm.

Routes:

| Route | Page |
| --- | --- |
| `GET /` | Cluster list. |
| `GET /clusters/{name}` | Cluster detail (nodes, current deployments, link to history). |
| `GET /history?cluster=X&service=Y` | Deploy history with optional filters. |
| `GET /healthz` | Liveness probe. Returns `200 ok`. |

Each page auto-refreshes every 30 seconds via `<meta http-equiv="refresh">`. There is no JS, no client-side state. `Ctrl-C` triggers a graceful 5-second shutdown.

---

## Architecture

The registry package mirrors the `internal/secrets/` pluggable-Provider pattern:

```
internal/registry/
  registry.go          Registry interface, ErrNotFound sentinel
  types.go             Cluster, Node, Deployment, DeploymentHistoryEntry, HistoryFilter
  factory.go           NewRegistry(ctx) selects backend by REGISTRY_BACKEND env
  sqlite/              SQLite Provider (default backend)
    sqlite.go
    init.go            Self-registers via factory.Register()
  migrations/
    migrations.go      ApplySQLite(ctx, *sql.DB)
    sqlite/0001_init.sql
    ff/.gitkeep        Reserved for the future FoundryFabric backend
  sync/
    sync.go            Reconciler used by `sync` and `diff`
```

To add another backend (for example a FoundryFabric-hosted registry shared across machines):

1. Create `internal/registry/<name>/` with a Provider implementing every method on the `Registry` interface.
2. Add `internal/registry/migrations/<name>/` for any DDL it needs.
3. In an `init()` register the constructor with the factory.
4. Set `REGISTRY_BACKEND=<name>` to switch.
