// manifests/otel-collector.jsonnet
// OpenTelemetry Collector — OTLP receiver (gRPC :4317, HTTP :4318),
// file exporter at /var/log/otel/traces.json.
// HostPath volume persists logs; daily CronJob purges files older than 7 days.

local lib = import 'github.com/FoundryFabric/jsonnet-lib/main.libsonnet';

local ns = 'otel';
local image = 'otel/opentelemetry-collector-contrib:0.99.0';
local logDir = '/var/log/otel';

local namespace = lib.namespace(ns);

local collectorConfig = |||
  receivers:
    otlp:
      protocols:
        grpc:
          endpoint: "0.0.0.0:4317"
        http:
          endpoint: "0.0.0.0:4318"

  processors:
    batch:
      timeout: 5s
      send_batch_size: 1000

  exporters:
    file:
      path: /var/log/otel/traces.json
      rotation:
        max_megabytes: 100
        max_days: 7
        max_backups: 3

  service:
    pipelines:
      traces:
        receivers: [otlp]
        processors: [batch]
        exporters: [file]
      metrics:
        receivers: [otlp]
        processors: [batch]
        exporters: [file]
      logs:
        receivers: [otlp]
        processors: [batch]
        exporters: [file]
|||;

local configMap = lib.configMap('otel-collector-config', ns, {
  'config.yaml': collectorConfig,
});

local labels = lib.labels('otel-collector');

local serviceAccount = lib.serviceAccount('otel-collector', ns);

local deployment = lib.deployment('otel-collector', ns, labels, {
  replicas: 1,
  selector: { matchLabels: labels },
  template: {
    metadata: { labels: labels },
    spec: {
      serviceAccountName: 'otel-collector',
      containers: [{
        name: 'otel-collector',
        image: image,
        imagePullPolicy: 'IfNotPresent',
        args: ['--config=/conf/config.yaml'],
        ports: [
          { name: 'otlp-grpc', containerPort: 4317, protocol: 'TCP' },
          { name: 'otlp-http', containerPort: 4318, protocol: 'TCP' },
        ],
        resources: {
          requests: { cpu: '100m', memory: '128Mi' },
          limits: { cpu: '500m', memory: '512Mi' },
        },
        volumeMounts: [
          {
            name: 'config',
            mountPath: '/conf',
            readOnly: true,
          },
          {
            name: 'otel-logs',
            mountPath: logDir,
          },
        ],
      }],
      volumes: [
        {
          name: 'config',
          configMap: { name: 'otel-collector-config' },
        },
        {
          name: 'otel-logs',
          hostPath: {
            path: logDir,
            type: 'DirectoryOrCreate',
          },
        },
      ],
    },
  },
});

local service = lib.service('otel-collector', ns, labels, [
  { name: 'otlp-grpc', port: 4317, targetPort: 4317, protocol: 'TCP' },
  { name: 'otlp-http', port: 4318, targetPort: 4318, protocol: 'TCP' },
]);

// Daily CronJob: delete files in logDir older than 7 days.
local logRotationCronJob = lib.cronJob(
  'otel-log-rotation',
  ns,
  '0 0 * * *',  // daily at midnight
  {
    spec: {
      template: {
        metadata: { labels: { app: 'otel-log-rotation' } },
        spec: {
          restartPolicy: 'OnFailure',
          containers: [{
            name: 'log-rotation',
            image: 'busybox:1.36',
            imagePullPolicy: 'IfNotPresent',
            command: [
              'sh', '-c',
              'find %s -type f -mtime +7 -delete && echo "Log rotation complete: deleted files older than 7 days"' % logDir,
            ],
            volumeMounts: [{
              name: 'otel-logs',
              mountPath: logDir,
            }],
          }],
          volumes: [{
            name: 'otel-logs',
            hostPath: {
              path: logDir,
              type: 'DirectoryOrCreate',
            },
          }],
        },
      },
    },
  }
);

[
  namespace,
  serviceAccount,
  configMap,
  deployment,
  service,
  logRotationCronJob,
]
