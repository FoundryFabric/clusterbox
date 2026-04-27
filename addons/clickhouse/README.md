# clickhouse addon

Installs the [Altinity ClickHouse Operator](https://github.com/Altinity/clickhouse-operator) `0.23.5` into the `clickhouse-operator` namespace.

The operator manages `ClickHouseInstallation` (CHI) and `ClickHouseKeeperInstallation` (CHKI) CRDs, letting you declaratively define ClickHouse clusters including sharding, replication, storage, and users.

## Install

```sh
clusterbox addon install clickhouse --cluster <name>
```

Optionally set an admin password first:

```sh
clusterbox secret set CLICKHOUSE_ADMIN_PASSWORD --cluster <name>
clusterbox addon install clickhouse --cluster <name>
```

## Create a ClickHouse instance

After the operator pod is running (`kubectl get pods -n clickhouse-operator`), apply a `ClickHouseInstallation` CR:

```yaml
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseInstallation
metadata:
  name: clickhouse
  namespace: clickhouse
spec:
  configuration:
    users:
      # Reference the secret created by clusterbox addon install.
      # Replace <password> with the value from clickhouse-admin-credentials.
      admin/password: "<CLICKHOUSE_ADMIN_PASSWORD>"
      admin/networks/ip:
        - "::/0"
    clusters:
      - name: local
        layout:
          shardsCount: 1
          replicasCount: 1
    storage:
      templates:
        volumeClaimTemplates:
          - name: data
            spec:
              accessModes:
                - ReadWriteOnce
              resources:
                requests:
                  storage: 10Gi
```

```sh
kubectl apply -f clickhouse-installation.yaml
```

## Access

```sh
# HTTP interface
kubectl port-forward svc/clickhouse-local 8123:8123 -n clickhouse

# Native protocol
kubectl port-forward svc/clickhouse-local 9000:9000 -n clickhouse
clickhouse-client --host localhost --port 9000 --user admin --password <password>
```

## Scale up

Edit the CHI and increase `shardsCount` / `replicasCount`. The operator handles rolling restarts automatically.

For replication across multiple shards you will also want a `ClickHouseKeeperInstallation` (replaces ZooKeeper); see the [Altinity docs](https://docs.altinity.com/clickhouseoperator/) for a worked example.
