# freebuff-proxy

Go wire gateway in front of FreeBuff, with OpenAI-compatible and Anthropic
endpoints plus an embedded Svelte dashboard.

## What it is

- Speaks OpenAI chat (`POST /v1/chat/completions`, `GET /v1/models`) and an
  Anthropic-compatible layer, then translates to the FreeBuff wire protocol.
- Runs in pooled, bridge, or hybrid mode (`EffectiveMode`):
  - **Pooled** — `AUTH_TOKENS` set + `BRIDGE_ENABLED=0`; pool only.
  - **Bridge** — `AUTH_TOKENS` empty; each request carries its own token.
  - **Hybrid** (default with `AUTH_TOKENS`) — `API_KEYS` credential uses the
    pool, any other credential relays upstream as a bridge token.
- Dashboard at `/admin` (Svelte SPA embedded in the binary).
- Freebucks metering follows the wire `prices` map: charged once per session-hour
  at session start, refunded on early `DELETE`, refilled on a Pacific-midnight
  cadence.

## Quickstart

```sh
cp .env.example .env   # then edit: AUTH_TOKENS, ADMIN_TOKEN, ...
go build ./backend/...
go run ./backend/cmd/freebuff-proxy
```

Run from GHCR (release image, no local build):

```sh
cp .env.example .env   # then edit: AUTH_TOKENS, ADMIN_TOKEN, ...
VERSION=v1.7.0 docker compose pull
VERSION=v1.7.0 docker compose up -d
```

Pin `VERSION` to the release tag; verify `GET /healthz` → 200, and note
`/admin` sits behind the login gate (redirects to `/admin/login`).

Then:

- `GET http://localhost:3457/healthz` → 200
- `GET http://localhost:3457/v1/models` → live model list
- `http://localhost:3457/admin` → dashboard

Defaults that matter (`.env.example`): `SAFE_MODE=true` (anti-ban preset),
`COST_MODE=free`, 30 req/min and 1500 req/day Pacific limits.

Configuration persistence: the first boot imports the effective config
(process env wins over `.env` over defaults) into the dashboard DB
(`data/freebuff.db`, mode `0600`) as `config:` overlay rows plus a
`config:migrated_env_v1` marker — later boots are no-ops via the marker.
The DB is then the persisted home the dashboard saves write to, secrets
included (`AUTH_TOKENS`, `ADMIN_TOKEN`, `API_KEYS`, `WEBHOOK_URL` rows);
keep its `0600` mode on copies/backups. Explicit process env still wins at
runtime, so a migrated row never overrides the environment.

## Update safety (read before every recreate)

Two-path layout: the live store is `/app/data/freebuff.db` on the `db_data`
named volume (`DB_PATH`, compose-level — an overlay row can never repoint
the open file), while the host checkout bind (`.:/app/state`, the working
directory) holds `.env`, logs, and the pre-volume bind DB at
`./data/freebuff.db`. A fresh volume auto-imports that bind DB on first
boot — display history plus the full operator state (settings overlay with
secrets, pages, sessions, tokens, pool blobs), per-table, idempotent,
secrets as opaque DB values — then later boots are strict no-ops. Legacy
files are never deleted. Never copy a live DB with plain `cp` of the
`.db`/`-wal`/`-shm` trio; stop first or use the backup script.

Every update runs three commands (any trip = roll back, never cut traffic):

```sh
docker compose stop freebuff-proxy
scripts/backup-state.sh                      # snapshot + count manifest
docker compose up -d --build                  # recreate on the same volume
ADMIN_TOKEN="$ADMIN_TOKEN" scripts/verify-state.sh   # healthz + 401 probe + migrate.noop + manifest counts
```

The gate requires `/healthz` 200, a wrong-token login 401, a strict no-op
boot (`migrate.fresh=false`, `migrate.noop=true`, `applied=[]`), and live
row counts matching the backup manifest (operator tables exact,
`pool_state` anti-stranding, history grow-only). First-ever volume adoption
boots `fresh=true` while it carries the bind DB — confirm the
`carried legacy state` log line against the manifest, restart once, then
the gate goes green.

## Layout

- `backend/` — gateway source.
- `frontend/` — dashboard SPA source.
- `scripts/` — upstream sync / drift tooling.
- `docs/` — agent workflow notes.

## Contributing

Protected `main`: branch → PR → green CI → squash merge, Conventional Commits.
See `AGENTS.md` for the full operating guide. Never commit secrets.
