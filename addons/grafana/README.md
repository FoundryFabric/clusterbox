# grafana addon

Installs [Grafana](https://grafana.com/grafana/) into the `monitoring` namespace.

## Install

```sh
clusterbox addon install grafana --cluster <name>
```

### Admin password

Set `GRAFANA_ADMIN_PASSWORD` in your secrets backend before installing to use a known password:

```sh
clusterbox secret set GRAFANA_ADMIN_PASSWORD --cluster <name>
clusterbox addon install grafana --cluster <name>
```

If the secret is not set, retrieve the auto-generated password after install:

```sh
kubectl get secret grafana-admin-credentials -n monitoring \
  -o jsonpath='{.data.admin-password}' | base64 -d
```

## Access

```sh
kubectl port-forward svc/grafana 3000:80 -n monitoring
```

Open [http://localhost:3000](http://localhost:3000) and log in as `admin`.
