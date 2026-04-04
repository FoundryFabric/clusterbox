# Secrets & Config

clusterbox resolves secrets before applying any manifest. This document covers how the system works, how to configure each backend, and how to add a new backend.

---

## How it works

When you run `clusterbox deploy <service> <version> --cluster <name>`:

1. **Resolve secrets** — read from the configured backend (see below)
2. **Create k8s Secret** — `kubectl create secret generic <service>-secrets --from-literal=KEY=VALUE ...`
3. **Apply manifest** — `kubectl apply -f manifest.yaml`
4. **Rollout** — `kubectl rollout status deployment/<service>`

Secrets never touch disk between steps 1 and 2. They are passed in-memory.

Similarly, `clusterbox up` creates `ghcr-credentials` (the imagePullSecret for `ghcr.io`) using the same resolution flow.

---

## Secret path convention

All secrets share a 5-segment logical path:

```
app / env / provider / region / key

e.g. foundryfabric / prod / hetzner / ash / JWT_SECRET
     kango         / dev  / hetzner / ash / KANGO_ADMIN_PASSWORD
```

Each backend maps this path to its own storage format (see below).

---

## Choosing a backend

Set `SECRETS_BACKEND` before running any clusterbox command:

| Value | Backend | When to use |
|---|---|---|
| `dev` (default) | JSON file | Local development |
| `onepassword` | 1Password Connect API or op CLI | Staging / production |
| `vault` | HashiCorp Vault KV v2 | Alternative production backend |

---

## Backend: `dev`

Reads `deploy/config/dev.secrets.json` from the service repo. The file is committed — values are throwaway dev credentials, not real secrets.

```json
{
  "JWT_SECRET": "dev-jwt-secret-not-for-production",
  "KANGO_ADMIN_EMAIL": "admin@localhost",
  "KANGO_ADMIN_PASSWORD": "dev-password"
}
```

No environment variables required. This is the default when `SECRETS_BACKEND` is unset.

---

## Backend: `onepassword`

Two modes, tried in order:

### Connect API mode (preferred for CI/CD)

Requires a [1Password Connect server](https://developer.1password.com/docs/connect/).

```bash
export SECRETS_BACKEND=onepassword
export OP_CONNECT_HOST=http://localhost:8080   # or your Connect server URL
export OP_CONNECT_TOKEN=<connect-server-token>
```

Path mapping:
```
app/env/provider/region/key
→ vault: <app>
→ item:  <env>-<provider>-<region>
→ field: <key>
```

Example: `foundryfabric/prod/hetzner/ash/JWT_SECRET`
→ vault `foundryfabric`, item `prod-hetzner-ash`, field `JWT_SECRET`

### CLI mode (fallback for local use)

If `OP_CONNECT_HOST` is not set, falls back to the `op` CLI:

```bash
export SECRETS_BACKEND=onepassword
export OP_SERVICE_ACCOUNT_TOKEN=<service-account-token>  # or run: op signin
```

Runs `op read "op://<app>/<env>-<provider>-<region>/<key>"` per secret.

### Setting up 1Password items

Create one item per environment per service, using this naming convention:

```
Vault name:  <app>               e.g. "foundryfabric"
Item title:  <env>-<provider>-<region>   e.g. "prod-hetzner-ash"
Fields:      one per secret key           e.g. JWT_SECRET, FF_BOOTSTRAP_KEY
```

See each service's `deploy/config/SECRETS.md` for the exact keys required.

---

## Backend: `vault`

Uses [HashiCorp Vault KV v2](https://developer.hashicorp.com/vault/docs/secrets/kv/kv-v2). No SDK dependency — raw HTTP only.

```bash
export SECRETS_BACKEND=vault
export VAULT_ADDR=http://vault.internal:8200

# Token auth (simple):
export VAULT_TOKEN=<root-or-policy-token>

# AppRole auth (recommended for CI/CD):
export VAULT_ROLE_ID=<role-id>
export VAULT_SECRET_ID=<secret-id>
```

Path mapping:
```
app/env/provider/region  →  secret/data/<app>/<env>/<provider>/<region>
```

All keys for a given app/env/provider/region are stored as fields in a single KV secret at that path. Example:

```bash
vault kv put secret/foundryfabric/prod/hetzner/ash \
  JWT_SECRET="..." \
  FF_BOOTSTRAP_KEY="..."
```

---

## ConfigMaps vs Secrets

clusterbox uses **Secrets** (not ConfigMaps) for all sensitive values. The distinction:

| | ConfigMap | Secret |
|---|---|---|
| **Use for** | Non-sensitive config (ports, feature flags, timeouts) | Passwords, tokens, keys, credentials |
| **Stored as** | Plain text in etcd | Base64-encoded in etcd (use Sealed Secrets or Vault agent for encryption at rest) |
| **In manifests** | `configMapKeyRef` | `secretKeyRef` |

Non-sensitive per-environment config (e.g. replica counts, resource limits, domain names) lives in the Jsonnet config files (`deploy/config/dev.jsonnet`, `deploy/config/prod-hetzner-ash.jsonnet`) and is rendered into the manifest at deploy time — no k8s ConfigMap needed.

---

## Adding a new backend

Implement the `Provider` interface in `internal/secrets/provider.go`:

```go
type Provider interface {
    Get(ctx context.Context, path SecretPath) (string, error)
    GetAll(ctx context.Context, prefix SecretPath) (map[string]string, error)
}
```

Then register it in `internal/secrets/factory.go`:

```go
case "mybackend":
    return mybackend.NewProvider(ctx)
```

Set `SECRETS_BACKEND=mybackend` and clusterbox will use it automatically.
