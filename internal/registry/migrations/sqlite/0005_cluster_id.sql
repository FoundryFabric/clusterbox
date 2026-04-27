PRAGMA foreign_keys = OFF;

-- New clusters table with surrogate integer PK
CREATE TABLE clusters_v2 (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  provider TEXT NOT NULL,
  region TEXT NOT NULL,
  env TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL,
  kubeconfig_path TEXT NOT NULL,
  last_synced_at TIMESTAMP,
  destroyed_at TIMESTAMP
);
INSERT INTO clusters_v2 (id, name, provider, region, env, created_at, kubeconfig_path, last_synced_at, destroyed_at)
SELECT rowid, name, provider, region, env, created_at, kubeconfig_path, last_synced_at, destroyed_at
FROM clusters;

-- Rebuild nodes with cluster_id FK
CREATE TABLE nodes_v2 (
  cluster_id INTEGER NOT NULL REFERENCES clusters_v2(id) ON DELETE CASCADE,
  hostname TEXT NOT NULL,
  role TEXT NOT NULL,
  joined_at TIMESTAMP NOT NULL,
  arch TEXT,
  os_version TEXT,
  k3s_version TEXT,
  agent_version TEXT,
  last_inspected_at TIMESTAMP,
  PRIMARY KEY (cluster_id, hostname)
);
INSERT INTO nodes_v2
SELECT c.id, n.hostname, n.role, n.joined_at, n.arch, n.os_version, n.k3s_version, n.agent_version, n.last_inspected_at
FROM nodes n JOIN clusters_v2 c ON c.name = n.cluster_name;

-- Rebuild deployments with cluster_id FK
CREATE TABLE deployments_v2 (
  cluster_id INTEGER NOT NULL REFERENCES clusters_v2(id) ON DELETE CASCADE,
  service TEXT NOT NULL,
  version TEXT NOT NULL,
  deployed_at TIMESTAMP NOT NULL,
  deployed_by TEXT NOT NULL,
  status TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'app',
  PRIMARY KEY (cluster_id, service)
);
INSERT INTO deployments_v2
SELECT c.id, d.service, d.version, d.deployed_at, d.deployed_by, d.status, d.kind
FROM deployments d JOIN clusters_v2 c ON c.name = d.cluster_name;

-- deployment_history: add cluster_id (no FK — audit log survives cluster lifetime), keep cluster_name for display
ALTER TABLE deployment_history ADD COLUMN cluster_id INTEGER;
UPDATE deployment_history SET cluster_id = (
  SELECT id FROM clusters_v2 WHERE name = deployment_history.cluster_name
);
DROP INDEX IF EXISTS idx_deployment_history_cluster_service;
CREATE INDEX idx_deployment_history_cluster_id ON deployment_history(cluster_id, attempted_at DESC);

-- Rebuild hetzner_resources with cluster_id FK
CREATE TABLE hetzner_resources_v2 (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  cluster_id INTEGER NOT NULL REFERENCES clusters_v2(id) ON DELETE CASCADE,
  resource_type TEXT NOT NULL,
  hetzner_id TEXT NOT NULL,
  hostname TEXT,
  created_at TIMESTAMP NOT NULL,
  destroyed_at TIMESTAMP,
  metadata TEXT
);
INSERT INTO hetzner_resources_v2 (id, cluster_id, resource_type, hetzner_id, hostname, created_at, destroyed_at, metadata)
SELECT r.id, c.id, r.resource_type, r.hetzner_id, r.hostname, r.created_at, r.destroyed_at, r.metadata
FROM hetzner_resources r JOIN clusters_v2 c ON c.name = r.cluster_name;

DROP TABLE hetzner_resources;
ALTER TABLE hetzner_resources_v2 RENAME TO hetzner_resources;
DROP TABLE deployments;
ALTER TABLE deployments_v2 RENAME TO deployments;
DROP TABLE nodes;
ALTER TABLE nodes_v2 RENAME TO nodes;
DROP TABLE clusters;
ALTER TABLE clusters_v2 RENAME TO clusters;

CREATE INDEX idx_hetzner_resources_cluster ON hetzner_resources(cluster_id, destroyed_at);
CREATE INDEX idx_hetzner_resources_type ON hetzner_resources(resource_type);
CREATE INDEX idx_deployments_kind ON deployments(kind);

PRAGMA foreign_keys = ON;
