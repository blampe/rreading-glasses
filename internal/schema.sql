CREATE TABLE IF NOT EXISTS "cache" (
  "key" TEXT NOT NULL PRIMARY KEY,
  "value" BYTEA NOT NULL,
  "expires" TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '7 day'
);
CREATE INDEX IF NOT EXISTS cache_expires_idx ON "cache" (expires);

