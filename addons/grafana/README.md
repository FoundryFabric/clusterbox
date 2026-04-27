# grafana addon

Installs the [Grafana Operator](https://github.com/grafana/grafana-operator) `v5.6.0` into the `grafana-operator` namespace.

The operator manages `Grafana`, `GrafanaDashboard`, `GrafanaDatasource`, and `GrafanaAlertRuleGroup` CRDs, letting you version-control your entire observability configuration as Kubernetes resources.

## Install

```sh
clusterbox addon install grafana --cluster <name>
```

Optionally set an admin password first:

```sh
clusterbox secret set GRAFANA_ADMIN_PASSWORD --cluster <name>
clusterbox addon install grafana --cluster <name>
```

## Create a Grafana instance

After the operator pod is running (`kubectl get pods -n grafana-operator`), apply a `Grafana` CR:

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: Grafana
metadata:
  name: grafana
  namespace: monitoring
  labels:
    dashboards: grafana   # used by GrafanaDashboard selectors
spec:
  config:
    log:
      mode: console
    security:
      admin_user: admin
  deployment:
    spec:
      template:
        spec:
          containers:
            - name: grafana
              # Reference the secret created by clusterbox addon install.
              envFrom:
                - secretRef:
                    name: grafana-admin-credentials
```

```sh
kubectl apply -f grafana-instance.yaml
```

## Add a datasource

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDatasource
metadata:
  name: prometheus
  namespace: monitoring
spec:
  instanceSelector:
    matchLabels:
      dashboards: grafana
  datasource:
    name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus-server.monitoring.svc.cluster.local
```

## Access

```sh
kubectl port-forward svc/grafana-service 3000:3000 -n monitoring
```

Open [http://localhost:3000](http://localhost:3000) and log in as `admin`.
