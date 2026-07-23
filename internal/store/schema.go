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
  credential_id   TEXT NOT NULL REFERENCES credentials(id) ON DELETE CASCADE,
  created_at      INTEGER NOT NULL,
  last_seen_at    INTEGER NOT NULL,
  request_count   INTEGER NOT NULL DEFAULT 0,
  status          TEXT NOT NULL DEFAULT 'active'
);

CREATE INDEX IF NOT EXISTS idx_conv_last_seen ON conversations(last_seen_at);
CREATE INDEX IF NOT EXISTS idx_creds_status   ON credentials(status);

CREATE TABLE IF NOT EXISTS rr_cursor (k INTEGER PRIMARY KEY, idx INTEGER NOT NULL);
INSERT OR IGNORE INTO rr_cursor (k, idx) VALUES (0, 0);

-- Named bearer tokens for multi-user authentication.
CREATE TABLE IF NOT EXISTS user_tokens (
  id           TEXT    PRIMARY KEY,
  name         TEXT    NOT NULL UNIQUE,
  token        TEXT    NOT NULL UNIQUE,
  status       TEXT    NOT NULL DEFAULT 'active',
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_user_tokens_token  ON user_tokens(token);
CREATE INDEX IF NOT EXISTS idx_user_tokens_status ON user_tokens(status);

-- One row per forwarded request for dashboard aggregation.
CREATE TABLE IF NOT EXISTS request_log (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  user_token_id  TEXT    REFERENCES user_tokens(id) ON DELETE SET NULL,
  credential_id  TEXT,
  conv_id        TEXT    NOT NULL DEFAULT '',
  ts             INTEGER NOT NULL,
  path           TEXT    NOT NULL DEFAULT '',
  status_code    INTEGER NOT NULL DEFAULT 0,
  bytes_sent     INTEGER NOT NULL DEFAULT 0,
  bytes_received INTEGER NOT NULL DEFAULT 0,
  latency_ms     INTEGER NOT NULL DEFAULT 0,
  model                 TEXT    NOT NULL DEFAULT '',
  input_tokens          INTEGER NOT NULL DEFAULT 0,
  output_tokens         INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_request_log_user_ts ON request_log(user_token_id, ts);
CREATE INDEX IF NOT EXISTS idx_request_log_ts      ON request_log(ts);

CREATE TABLE IF NOT EXISTS usage_history (
  id                          INTEGER PRIMARY KEY AUTOINCREMENT,
  credential_id               TEXT    NOT NULL REFERENCES credentials(id) ON DELETE CASCADE,
  captured_at                 INTEGER NOT NULL,
  five_hour_pct               REAL    NOT NULL DEFAULT 0,
  five_hour_resets_at         INTEGER,
  seven_day_pct               REAL    NOT NULL DEFAULT 0,
  seven_day_resets_at         INTEGER,
  seven_day_sonnet_pct        REAL    NOT NULL DEFAULT 0,
  seven_day_sonnet_resets_at  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_usage_history_cred_time
  ON usage_history(credential_id, captured_at);
`
