// manifests/fdb-operator.jsonnet
// FoundationDB Kubernetes Operator v2.24.0 — single-node dev cluster.
//
// Renders: Namespace, CRD (FoundationDBCluster), ServiceAccount,
// ClusterRole, ClusterRoleBinding, Operator Deployment, FdbCluster CR.
// No NodePort or LoadBalancer services — ClusterIP only.

local lib = import 'github.com/FoundryFabric/jsonnet-lib/main.libsonnet';

local ns = 'fdb-system';
local operatorImage = 'foundationdb/fdb-kubernetes-operator:v2.24.0';
local operatorName = 'fdb-kubernetes-operator-controller-manager';

local namespace = lib.namespace(ns);

local crd = {
  apiVersion: 'apiextensions.k8s.io/v1',
  kind: 'CustomResourceDefinition',
  metadata: {
    name: 'foundationdbclusters.apps.foundationdb.org',
  },
  spec: {
    group: 'apps.foundationdb.org',
    names: {
      kind: 'FoundationDBCluster',
      listKind: 'FoundationDBClusterList',
      plural: 'foundationdbclusters',
      singular: 'foundationdbcluster',
      shortNames: ['fdb'],
    },
    scope: 'Namespaced',
    versions: [{
      name: 'v1beta2',
      served: true,
      storage: true,
      schema: {
        openAPIV3Schema: {
          type: 'object',
          properties: {
            spec: { type: 'object', 'x-kubernetes-preserve-unknown-fields': true },
            status: { type: 'object', 'x-kubernetes-preserve-unknown-fields': true },
          },
        },
      },
    }],
  },
};

local serviceAccount = lib.serviceAccount(operatorName, ns);

local clusterRole = lib.clusterRole('fdb-operator-role', [
  {
    apiGroups: ['apps.foundationdb.org'],
    resources: ['foundationdbclusters', 'foundationdbclusters/status', 'foundationdbclusters/finalizers'],
    verbs: ['get', 'list', 'watch', 'create', 'update', 'patch', 'delete'],
  },
  {
    apiGroups: [''],
    resources: ['pods', 'services', 'configmaps', 'secrets', 'persistentvolumeclaims'],
    verbs: ['get', 'list', 'watch', 'create', 'update', 'patch', 'delete'],
  },
  {
    apiGroups: ['apps'],
    resources: ['deployments', 'statefulsets'],
    verbs: ['get', 'list', 'watch', 'create', 'update', 'patch', 'delete'],
  },
]);

local clusterRoleBinding = lib.clusterRoleBinding('fdb-operator-rolebinding', {
  apiGroup: 'rbac.authorization.k8s.io',
  kind: 'ClusterRole',
  name: 'fdb-operator-role',
}, [{
  kind: 'ServiceAccount',
  name: operatorName,
  namespace: ns,
}]);

local labels = lib.labels('fdb-kubernetes-operator', 'controller-manager');

local deployment = lib.deployment(operatorName, ns, labels, {
  replicas: 1,
  selector: { matchLabels: labels },
  template: {
    metadata: { labels: labels },
    spec: {
      serviceAccountName: operatorName,
      containers: [{
        name: 'manager',
        image: operatorImage,
        imagePullPolicy: 'IfNotPresent',
        command: ['/manager'],
        args: ['--enable-leader-election'],
        env: [{
          name: 'WATCH_NAMESPACE',
          valueFrom: { fieldRef: { fieldPath: 'metadata.namespace' } },
        }],
        resources: {
          requests: { cpu: '100m', memory: '128Mi' },
          limits: { cpu: '500m', memory: '512Mi' },
        },
        securityContext: {
          allowPrivilegeEscalation: false,
          readOnlyRootFilesystem: true,
        },
      }],
      securityContext: {
        runAsNonRoot: true,
      },
      terminationGracePeriodSeconds: 10,
    },
  },
});

// Single-node dev FdbCluster CR — no external exposure.
local fdbCluster = {
  apiVersion: 'apps.foundationdb.org/v1beta2',
  kind: 'FoundationDBCluster',
  metadata: {
    name: 'foundationdb',
    namespace: ns,
  },
  spec: {
    version: '7.4.6',
    processCounts: {
      stateless: 1,
      log: 1,
      storage: 1,
    },
    // ClusterIP service only — no external exposure.
    services: {
      type: 'ClusterIP',
      headless: true,
    },
  },
};

// Output as a JSON array of all resources for `kubectl apply -f`.
[
  namespace,
  crd,
  serviceAccount,
  clusterRole,
  clusterRoleBinding,
  deployment,
  fdbCluster,
]
