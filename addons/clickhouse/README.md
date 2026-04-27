# clickhouse addon

Installs [ClickHouse](https://clickhouse.com/) (via Bitnami chart) into the `clickhouse` namespace.

Configured as a single-shard, single-replica instance — suitable for development and staging. Scale up `shards` and `replicaCount` via a values override for production.

## Install

```sh
clusterbox addon install clickhouse --cluster <name>
```

### Admin password

Set `CLICKHOUSE_ADMIN_PASSWORD` in your secrets backend before installing:

```sh
clusterbox secret set CLICKHOUSE_ADMIN_PASSWORD --cluster <name>
clusterbox addon install clickhouse --cluster <name>
```

If the secret is not set, retrieve the auto-generated password after install:

```sh
kubectl get secret clickhouse-admin-credentials -n clickhouse \
  -o jsonpath='{.data.admin-password}' | base64 -d
```

## Access

```sh
kubectl port-forward svc/clickhouse 8123:8123 -n clickhouse
```

Connect via HTTP at `http://localhost:8123` or native protocol at port `9000`:

```sh
kubectl port-forward svc/clickhouse 9000:9000 -n clickhouse
clickhouse-client --host localhost --port 9000 --user admin --password <password>
```
