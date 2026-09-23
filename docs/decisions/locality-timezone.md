# Session locality — the timezone the gateway declares for the account's reset zone

Status: **shipped**. The resolver is `backend/internal/egress/locality.go`
(rule + country→zone table), the background probe is
`backend/internal/egress/tracker.go`, and the wiring is the upstream resolver
(`backend/internal/upstream/client.go`, `session.go`), the server's
`SetEgressTracker` (`backend/internal/server/server_init.go`), the serve path
(`backend/internal/cli/cli_serve.go`), and the doctor row
(`backend/internal/cli/doctor/doctor.go`). The knob is `SESSION_TIMEZONE`
(`backend/internal/config/keycatalog.go`).

## The vendor fact this is built on

The wire has **no client-settable country**. `common/src/constants/freebuff-models.ts`
(gitignored vendor clone; the in-repo mirror is
`backend/internal/registry/testdata/upstream/freebuff-models.ts`) states that the
country is resolved server-side from the authenticated request, because a
client-chosen country is one an abusive client rotates. So a region can only be
*observed* (the egress IP the server sees), never declared.

The only locality the client declares is the timezone header.
`common/src/util/freebucks-timezone.ts` (gitignored vendor clone) exports
`FREEBUCKS_TIMEZONE_HEADER` = `x-fb-timezone`, and the repo's own copy of the
contract is `backend/internal/upstream/session.go:33-42`: a timezone is a
**scheduling preference, never proof of country or access**.
`cli/src/utils/freebuff-session-api.ts` spreads `freebucksTimeZoneHeaders()`
into **every** session call — admission POST, poll GET, probe GET, and the
DELETE/refund — so the declaration is unconditional, not a probe-only flourish
(`session.go:456-457` stamps it at the `sessionCall` chokepoint, `:281-288` on
the DELETE, which returns a receipt rather than a `SessionState` and therefore
bypasses that chokepoint).

## Consequence: the declared zone picks the account's reset zone

The upstream server derives the account's daily reset zone (`daily.resetAt`)
from `x-fb-timezone`. Declaring the *host's* zone is therefore wrong for the
deployment this project actually runs: a VPS clock left at UTC (or a container
with `time.Local == "Local"`) declares a zone nobody chose, which is exactly the
boring case the region rule replaces.

## The rule (owner's decision)

Priority order, `backend/internal/egress/locality.go:203-237`:

| Order | Condition | Result | Source |
|---|---|---|---|
| 1 | `SESSION_TIMEZONE` set and loadable | the override | `override` |
| 2 | host zone loadable and **not boring** | the host zone | `host` |
| 3 | egress country maps to a zone | that zone | `region` |
| 4 | host zone loadable but boring | the host zone (UTC and friends) | `host` |
| 5 | otherwise | `UTC` | `utc` |

`BoringZone` (`locality.go:149-169`) is the whole point: empty, `Local`, and the
UTC/GMT class (`UTC`, `GMT`, `Etc/UTC`, `Etc/GMT`, `Etc/GMT±N`) carry no
locality, so they do not out-rank detection. A *real* host zone is a deliberate
choice and always wins; the region only fills the silence. `ValidZone`
(`locality.go:190-201`) is `time.LoadLocation`; the binary gets tzdata from
`cli/cli_serve.go` (`_ "time/tzdata"`), so a minimal image still resolves the
table's zones.

### The table is lossy on purpose — the knob is the escape hatch

`countryZones` (`locality.go:38-127`) maps one country to **one** representative
zone (the most populous, and the comment above each multi-zone entry says so).
The wire carries exactly one timezone string and the server only uses it to pick
a reset day, so "which zone inside the country" has to be plausible, not exact.
An operator who needs the precise zone sets `SESSION_TIMEZONE` — which is why
the override ranks above everything, and why the region branch is a default, not
a claim.

## Fail-open behaviour

The tracker never blocks a request. `Tracker.Country()` (`tracker.go:100-113`)
serves its own snapshot first (kept across a cache TTL boundary and across a
failed probe), then the shared cache, and never probes. A failed probe is
fail-open at every layer: the tracker keeps the last detected country
(`tracker.go:85-98`), the resolver falls back down the priority chain, and the
gateway still declares a valid zone (host or UTC). The probe itself is one
`cdn-cgi/trace` GET (`egress/probe.go`), immediate at boot then every 10 minutes
(`DefaultTTL`), through a plain direct dialer; it stops with the process context.

An unrecognised-but-well-formed country, a malformed one, or a failed probe all
reach the same place: the host zone when it is real, else UTC. The country is
reported as `""` when unknown, and the zone branch reports `utc`.

## Privacy: country on `/healthz`, never the IP

`/healthz` is **unauthenticated**, so it gains three additive fields —
`session_timezone`, `session_timezone_source`, `egress_region`
(`backend/internal/server/health.go:117-134`) — and deliberately **not** the
probe's public IP. The doctor, which the operator runs deliberately and locally,
keeps the IP: `Egress region: <country> (<ip>)` is byte-identical
(`doctor/doctor.go:26-36`, pinned by `doctor_test.go:51-70`), with the new
`Session timezone: <zone> (<source>)` line beside it (`doctor.go:38-46`, printed
at `:233-234`).

## Knobs and defaults

| Knob | Default | Meaning |
|---|---|---|
| `SESSION_TIMEZONE` | `""` | The declared zone. Empty = auto (the rule above). An invalid value falls back to auto and warns once (`server_init.go:201-213`) — it is never a load error, because the loader accepts any text and a typo must not stop the gateway. |
| probe interval | `DefaultTTL` (10m) | `egress.NewTracker(..., egress.DefaultTTL)` in `cli_serve.go:427-432`; non-positive means the same 10m. |

`SESSION_TIMEZONE` is live: the resolver closure reads `s.cfg.Load()`
(`server_init.go:219-229`), so a dashboard save or `/admin/reload` changes the
declared zone on the next session call without a restart. The tracker is
started only from the serve path (`cli_serve.go:448-457`), never from a server
constructor, so tests that build a `Server` open no sockets; tests point
`egress.ProbeURL` at `httptest` when they exercise the probe.

## Files it touches

- `backend/internal/egress/locality.go`, `tracker.go` — the rule, the table, the
  probe loop (no server imports; the table is data).
- `backend/internal/upstream/client.go` (`SetLocalityResolver`), `session.go`
  (the chokepoint + the DELETE).
- `backend/internal/pool/pool.go` (`SetLocalityResolver`, `applyLocality`) — the
  pool owns the clients, so it is the fan-out point for fixed, runtime-added, and
  bridge clients.
- `backend/internal/server/server_init.go` (`SetEgressTracker`,
  `sessionLocality`, `applyConfig`), `health.go`.
- `backend/internal/cli/cli_serve.go` (tracker construction + start), `doctor/doctor.go`.
- `backend/internal/config/{config.go,config_keys.go,config_load.go,data.go,keycatalog.go}`,
  `backend/internal/server/admin_env.go` (the live/diff map), `.env.example:81-86`.

## Verification

- `go test ./backend/internal/egress/` — the rule's priority, the boring-zone
  class, the table, and the tracker's snapshot/fail-open semantics.
- `go test ./backend/internal/upstream/` —
  `TestProbeAccountSendsCLIParityHeaders` still pins the host zone with no
  resolver installed, and the resolver tests pin the declared zone on the
  admission POST and the poll GET.
- `go test ./backend/internal/server/ -run 'SessionLocality|Healthz'` — the
  additive `/healthz` fields and the override/region/host/UTC outcomes.
- `go test ./backend/internal/cli/...` — the doctor row, including the
  byte-identical `Egress region:` line.
