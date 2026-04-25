ALTER TABLE deployments ADD COLUMN kind TEXT NOT NULL DEFAULT 'app';
ALTER TABLE deployment_history ADD COLUMN kind TEXT NOT NULL DEFAULT 'app';
CREATE INDEX idx_deployments_kind ON deployments(kind);
