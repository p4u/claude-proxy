package store

const schema = `
CREATE TABLE IF NOT EXISTS credentials (
  id                TEXT PRIMARY KEY,
  label             TEXT,
  subscription_type TEXT,
  access_token      TEXT NOT NULL,
  refresh_token     TEXT NOT NULL,
  expires_at        INTEGER NOT NULL,
  status            TEXT NOT NULL,
  retry_after       INTEGER,
  last_success_at   INTEGER,
  last_429_at       INTEGER,
  last_request_at   INTEGER,
  request_count     INTEGER NOT NULL DEFAULT 0,
  success_count     INTEGER NOT NULL DEFAULT 0,
  error_count       INTEGER NOT NULL DEFAULT 0,
  weight            INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS conversations (
  id              TEXT PRIMARY KEY,
  credential_id   TEXT NOT NULL REFERENCES credentials(id),
  created_at      INTEGER NOT NULL,
  last_seen_at    INTEGER NOT NULL,
  request_count   INTEGER NOT NULL DEFAULT 0,
  status          TEXT NOT NULL DEFAULT 'active'
);

CREATE INDEX IF NOT EXISTS idx_conv_last_seen ON conversations(last_seen_at);
CREATE INDEX IF NOT EXISTS idx_creds_status   ON credentials(status);

CREATE TABLE IF NOT EXISTS rr_cursor (k INTEGER PRIMARY KEY, idx INTEGER NOT NULL);
INSERT OR IGNORE INTO rr_cursor (k, idx) VALUES (0, 0);

CREATE TABLE IF NOT EXISTS agent_sessions (
  conversation_key TEXT PRIMARY KEY,
  session_uuid     TEXT NOT NULL,
  created_at       INTEGER NOT NULL,
  last_used_at     INTEGER NOT NULL,
  num_turns        INTEGER NOT NULL DEFAULT 0,
  total_cost_usd   REAL    NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS users (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT    NOT NULL UNIQUE,
  token_sha256 TEXT    NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER
);
`
