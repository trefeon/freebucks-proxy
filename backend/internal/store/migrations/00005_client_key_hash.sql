-- 00005_client_key_hash: per-client API-key identity on request records
-- (issue #604): which pooled key served each /v1 inference. The identity is
-- hex(sha256(rawKey))[:16] — raw keys never reach this table — and "" for
-- bridge/no-key requests. NOT NULL DEFAULT '' so pre-migration rows read
-- back as "" with zero data change; the legacy carry (history.go) selects
-- the pre-00005 columns explicitly for old files.
--
-- +goose Up
ALTER TABLE request_records ADD COLUMN client_key_hash TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE request_records DROP COLUMN client_key_hash;
