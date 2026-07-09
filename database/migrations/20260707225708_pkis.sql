-- +goose Up
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    tg_id INTEGER UNIQUE,
    tg_username TEXT,
    tg_first_name TEXT,
    invite_code TEXT UNIQUE NOT NULL,
    can_invite BOOLEAN NOT NULL DEFAULT FALSE,
    active BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
ALTER TABLE items ADD COLUMN user_id TEXT NOT NULL REFERENCES users(id);

-- +goose Down
ALTER TABLE items DROP COLUMN user_id;
DROP TABLE users;
