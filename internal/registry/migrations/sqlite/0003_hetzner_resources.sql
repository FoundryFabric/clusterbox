CREATE TABLE hetzner_resources (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  cluster_name TEXT NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
  resource_type TEXT NOT NULL,
  hetzner_id TEXT NOT NULL,
  hostname TEXT,
  created_at TIMESTAMP NOT NULL,
  destroyed_at TIMESTAMP,
  metadata TEXT
);
CREATE INDEX idx_hetzner_resources_cluster ON hetzner_resources(cluster_name, destroyed_at);
CREATE INDEX idx_hetzner_resources_type ON hetzner_resources(resource_type);
ALTER TABLE clusters ADD COLUMN destroyed_at TIMESTAMP;
