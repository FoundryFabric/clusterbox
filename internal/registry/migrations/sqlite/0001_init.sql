CREATE TABLE clusters (
  name TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  region TEXT NOT NULL,
  env TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  kubeconfig_path TEXT NOT NULL,
  last_synced_at TIMESTAMP
);
CREATE TABLE nodes (
  cluster_name TEXT NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
  hostname TEXT NOT NULL,
  role TEXT NOT NULL,
  joined_at TIMESTAMP NOT NULL,
  PRIMARY KEY (cluster_name, hostname)
);
CREATE TABLE deployments (
  cluster_name TEXT NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
  service TEXT NOT NULL,
  version TEXT NOT NULL,
  deployed_at TIMESTAMP NOT NULL,
  deployed_by TEXT NOT NULL,
  status TEXT NOT NULL,
  PRIMARY KEY (cluster_name, service)
);
CREATE TABLE deployment_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  cluster_name TEXT NOT NULL,
  service TEXT NOT NULL,
  version TEXT NOT NULL,
  attempted_at TIMESTAMP NOT NULL,
  status TEXT NOT NULL,
  rollout_duration_ms INTEGER NOT NULL,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_deployment_history_cluster_service ON deployment_history(cluster_name, service, attempted_at DESC);
CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER NOT NULL
);
