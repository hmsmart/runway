-- +goose Up
ALTER TABLE users ADD COLUMN api_key TEXT;
CREATE UNIQUE INDEX idx_users_api_key ON users(api_key) WHERE api_key IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_users_api_key;
ALTER TABLE users DROP COLUMN api_key;
