-- +goose Up
ALTER TABLE accounts ADD COLUMN tracked INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE accounts DROP COLUMN tracked;
