# opentelemetry addon

Installs the [OpenTelemetry Operator](https://github.com/open-telemetry/opentelemetry-operator) into the `opentelemetry-operator-system` namespace.

The operator manages `OpenTelemetryCollector` and `Instrumentation` CRDs, letting you deploy collectors and enable auto-instrumentation without touching application code.

## Install

```sh
clusterbox addon install opentelemetry --cluster <name>
```

No secrets required.

## Deploy a collector

After the operator is ready, create an `OpenTelemetryCollector` resource:

```yaml
apiVersion: opentelemetry.io/v1alpha1
kind: OpenTelemetryCollector
metadata:
  name: otel
  namespace: default
spec:
  config: |
    receivers:
      otlp:
        protocols:
          grpc:
          http:
    exporters:
      logging:
        verbosity: detailed
    service:
      pipelines:
        traces:
          receivers: [otlp]
          exporters: [logging]
```

## Access

```sh
kubectl port-forward svc/otel-collector 4317:4317 -n default
```

Send traces to `localhost:4317` (gRPC) or `localhost:4318` (HTTP).
