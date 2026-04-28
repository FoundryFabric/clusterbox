PRAGMA foreign_keys = OFF;

DROP TABLE IF EXISTS hetzner_resources;

CREATE TABLE cluster_resources (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  cluster_id INTEGER NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  resource_type TEXT NOT NULL,
  external_id TEXT NOT NULL,
  hostname TEXT,
  created_at TIMESTAMP NOT NULL,
  destroyed_at TIMESTAMP,
  metadata TEXT
);

CREATE INDEX idx_cluster_resources_cluster ON cluster_resources(cluster_id, destroyed_at);
CREATE INDEX idx_cluster_resources_provider ON cluster_resources(provider, resource_type);

PRAGMA foreign_keys = ON;
