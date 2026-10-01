# Streaming performance

Where the proxy's per-frame cost goes, what was optimized, and which knobs
bound first-token latency. Numbers are from the in-repo benchmarks on this
host (11th Gen i7-11800H), Go 1.26, `-benchmem`.

## 1. Where latency goes

`phasetiming` records the phases of every chat request (`backend/internal/phasetiming`):

| phase | meaning |
|---|---|
| `acquire_ms` | whole `pool.Acquire` call |
| `session_refresh_ms` | upstream session admission inside Acquire |
| `run_acquire_ms` | run-slot acquire |
| `queue_wait_ms` | time parked in an account's live-turn queue |
| `upstream_ttfb_ms` | from the upstream chat call returning until the first relayed chunk |
| `total_ms` | whole request |

For a warm session the acquire phases are small; `upstream_ttfb_ms` and the
onwards stream rate are dominated by the upstream model, not the proxy. The
proxy's lever is therefore **per-chunk CPU/allocation** (aggregate throughput
and GC pressure under concurrency) plus a handful of **transport/config knobs**
below. Single-stream TTFT cannot be meaningfully reduced past the network and
the upstream's own time-to-first-token.

## 2. What was optimized: one decode per chunk

Every streaming relay used to decode the same upstream frame **twice**:

1. `convert.SanitizeChunkOpts` did `json.Unmarshal` to sanitize, then returned
   bytes.
2. The relay did `json.Unmarshal(clean, &chunk)` again to inspect/rewrite.

Both were on the critical path for every frame. `convert.SanitizeChunkMapped`
(backend/internal/convert/sse.go) returns the sanitized bytes **and** the
decoded map, so the relays decode once:

- OpenAI: `openai_stream.go` + `openai_chunk_pipeline.go` (`rewrite` now takes
  the already-decoded map instead of re-parsing `clean`).
- Anthropic: `anthropic_stream.go`.
- Responses: `responses_stream.go`.

Contract: `clean` is byte-for-byte what `SanitizeChunkOpts` returned; `chunk`
carries JSON number semantics (`float64`) on both the fast and sanitize paths,
so existing `.(float64)` assertions in the pipeline are unchanged.

### Measurements

Per-chunk decode (`BenchmarkSanitizeChunkDecode`, `-benchmem`):

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| before (SanitizeChunkOpts + json.Unmarshal) | ~7.7µs | 2755 | 70 |
| after  (SanitizeChunkMapped)                | ~4.1µs | 1544 | 36 |

≈ 47% less CPU, 44% fewer bytes, 49% fewer allocations per streamed chunk.

End-to-end OpenAI relay over 200 plain-text chunks
(`BenchmarkRelayStream`): ~1.30 ms/stream, ~154k chunks/s single-core, 15.3 MB/s,
7859 allocs/stream.

Run them:

```sh
go test ./backend/internal/server/ -run '^$' -bench 'RelayStream|SanitizeChunkDecode' -benchmem
go test ./backend/internal/convert/ -run '^$' -bench SanitizeChunk -benchmem
```

## 3. Knobs that bound first-token latency

These are the real single-request latency levers; the defaults keep them
deliberate.

- **`REQUEST_JITTER`** — random delay in `[0, REQUEST_JITTER)` before *every*
  upstream chat call (`upstream/chat.go`). Default `0`; `SAFE_MODE=true` sets
  200 ms and `.env.example` ships `200ms`. This is the single largest
  proxy-side TTFT cost when set. `REQUEST_JITTER=0` removes it; it is an
  anti-ban tradeoff, not a free win.
- **`HTTP2_UPSTREAM`** — `true` (default) negotiates HTTP/2 so the ALPN matches
  a real browser. `false` forces HTTP/1.1: separate connections can stream a
  burstier single stream under TCP head-of-line blocking, at the cost of an
  ALPN fingerprint mismatch. Only flip with a capture proving it helps.
- **Connection pool** — `upstream/client.go` keeps `MaxIdleConnsPerHost=64`,
  `IdleConnTimeout=120s`, 32 KiB read/write buffers, so warm sessions avoid
  per-message TLS handshakes on the high-BDP link.
- **`REQUEST_TIMEOUT`** — bounds only the wait for response headers
  (`ResponseHeaderTimeout`), never the streamed body, so a long turn cannot be
  cut mid-stream.
- **Keepalive** — the relay emits an SSE keepalive every 15 s of client-write
  silence (`keepaliveInterval`); it never blocks a frame.

## 4. Deliberately not changed

- **No `Accept-Encoding` / gzip tweaks upstream.** `newRequest` sends no
  browser `Accept-Encoding` (CLI fidelity); the Go transport's default gzip
  handling plus `wrapDecompress` are left intact. Changing this would move the
  request away from the captured CLI envelope (ban risk) for an unproven win.
- **No `map[string]any` → hand-rolled decoder rewrite.** The remaining
  ~36 allocs/chunk are JSON decoding of a generic object; a typed/streaming
  decoder is possible but touches every rewrite stage and the conformance
  surface, and is not justified by the measured end-to-end cost.
