# MASQ — Minimal Account Slot Queue Implementation Plan

> Mechanism: Ordered Queue-Spill (with Precious Holders). MASQ is the official queue name; Ordered Queue-Spill remains the mechanism name in docs.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ganti kontrol pool freebuff-proxy menjadi MASQ (Minimal Account Slot Queue): order ketat Account #1→#N, 2 slot concurrent per (akun, model) — satu akun boleh memegang 2×modelA + 2×modelB bersamaan, antrean FIFO per (akun, model) dengan spill ke akun next hanya saat queue-wait habis, precious session tidak pernah di-drop, PIN_MODEL strict 1 akun = 1 model, dan 429 natural membuat request kembali antre tanpa memarkir akun.

**Architecture:** Tiga fase berurutan di atas satu branch. Fase E memangkas semua limiter buatan (failover walk korelatif, cooldown ip_capped, unfit global 5m, COOLDOWN_* bounded, probe/maturity proaktif, scorer/rotasi/leader-election, retry-once chat, park/sweep) hingga yang tersisa hanya admission alami + antrean. Fase I membangun mekanisme baru (slot ledger, ordered lanes + spill, precious open-set, PIN_MODEL, 429-requeue) di atas sisa itu. Fase C mengunci order ketat, knobs final, dan UI PIN. Tiap fase berakhir dengan pool yang tetap kompilasi dan diuji throwaway.

**Tech Stack:** Go (backend/internal/pool, backend/internal/config, backend/internal/server, backend/internal/session, backend/internal/upstream klasifikasi saja), Svelte (frontend/src/lib/pages + components + utils), throwaway Go tests memakai harness yang sudah ada (`newTestPoolCfg`, `testutil.NewMock`, `SessionHandler`, `RequestsSnapshot`).

**Spec:** Proposal "Ordered Queue-Spill with precious holders" (approved 2026-09-17, R1–R6) + tabel limiter natural UpstreamLimits + alur CurrentControl. Catatan recovery: seksi desain-1/2/3 dari lane SlotOverflowDesign tidak terbaca (payload agen null; final yield lane itu terpotong limit token), jadi plan ini di-grounding langsung dari R1–R6 di kontrak batch, payload UpstreamLimits/CurrentControl yang pulih penuh, dan baca langsung kode pada revisi worktree ini (`origin/main` 328261be). Semua `file:line` di bawah diverifikasi pada revisi itu.

## Global Constraints

- Shared checkout `D:/github_repo/freebuff-proxy` berisi uncommitted user work: JANGAN sentuh. Semua kerja di worktree ini + branch `plan/ordered-queue-spill` saja.
- Tanpa build/lint/test project-wide mid-flight. Bukti per task hanya via throwaway test file task itu sendiri (`go test ./backend/internal/pool/ -run TestNama -count=1`), dihapus sebelum fase berikutnya bila tidak dipromosi jadi keeper.
- Secrets: jangan dump email/token/session-id di log/test; sanitasi jadi Account #N.
- Bahasa komentar kode: Inggris (konvensi repo). Bahasa UI: ikuti pola `$tr(...)` yang ada.
- Clean cutover: tiap penghapusan knob memigrasi setiap caller + test + UI + keycatalog; tanpa shim/alias/deprecated path. Satu-satunya alias yang diizinkan: TIDAK ADA — `TOKEN_MAX_CONCURRENT` di-rename total menjadi `SLOTS_PER_ACCOUNT`.
- R1–R6 adalah acceptance final; pemetaan tiap R ke task pelaksana ada di tabel §Traceability, dan tiap task perilaku membawa blok Repro berisi kode aktual.

**Knobs final (satu-satunya yang hidup setelah Fase C):**

| Knob | Default | Arti |
|---|---|---|
| `SLOTS_PER_ACCOUNT` | `2` | 2 slot concurrent per (akun, model) — satu akun boleh memegang 2×modelA + 2×modelB bersamaan (rename total `TOKEN_MAX_CONCURRENT`) |
| `QUEUE_WAIT` | `30s` | total parkir per lane sebelum spill; tanpa per-model override (satu writer) |
| `QUEUE_DEPTH` | `16` | waiter parkir per akun; `0` = tanpa antre |
| `PIN_MODEL` | `""` | pin strict `idx:model` satu model per akun, mis. `"0:z-ai/glm-5.2"` |
| `MAX_SPILL_ACCOUNTS` | `0` | batas akun lanjutan per request; `0` = tanpa batas (rantai index penuh) |

**Knobs yang mati (dihapus total beserta UI + test + keycatalog):** `TOKEN_ROTATION`, `ROUTING_SMART`, `RATE_LIMIT_FAILOVER`, `MODEL_LOCKS` (diganti `PIN_MODEL`), `TOKEN_MAX_CONCURRENT` (nama lama), semua `COOLDOWN_*_MS` + `COOLDOWN_IP_*`, `SESSION_PARK_*`, `SESSION_POLL_MAX_MS` (pacing poll di-hardcode 20s base / 5m cap sesuai port vendor di `backend/internal/session/session_park.go:35-38`), `SMART_PROBE_BACKOFF_MAX_MS`, `MATURITY_*`, `QUOTA_PROBE_*`, `MATURITY_TOUCH_MODEL`.

---

## File Structure

### Fase E — Excise (buang limiter buatan; tiap file satu tanggung jawab hapus)

- Modify `backend/internal/pool/acquire_route.go` — E1: potong failover walk untuk refusal korelatif (surface langsung).
- Modify `backend/internal/pool/cooldown.go` — E2: hapus `CooldownTokenIpCapped` + klasifikasi ip_capped di `classifyAndCooldown`; E4: hapus bounded-backoff umum (pertahankan ban/quarantine).
- Modify `backend/internal/runs/cooldown.go` — E2/E4: hapus sisi runs untuk ip_capped + bounded const (auth 30m, country 15m, ceiling 7d).
- Delete `backend/internal/pool/unfit.go` (+ `unfit_test.go`) — E3: hapus registry unfit global.
- Modify `backend/internal/upstream/classify.go` — E4: hapus `COOLDOWN_*` bounded const, ganti passthrough RetryAfter apa adanya.
- Delete `backend/internal/config/cooldown.go` — E4: hapus accessor + konstanta (kecuali yang dipertahankan untuk ban park bila ada; default: hapus total).
- Delete `backend/internal/pool/quota_smartprobe.go` (+ `quota_smartprobe_test.go`, `quota_smartprobe_fleet_test.go`, `quota_autoprobe_test.go`, `smartprobe_persist_test.go`) — E5.
- Delete `backend/internal/pool/quota_visitprobe.go` (+ `quota_visitprobe_test.go`) — E5.
- Delete `backend/internal/pool/quota_bootseed.go` (+ `quota_bootseed_test.go`) — E5.
- Delete `backend/internal/pool/maturity.go` (+ `maturity_test.go`, `maturity_auto_test.go`, `maturity_firegate_test.go`, `maturity_persist_test.go`, `maturity_resultday_test.go`, `maturity_skip_loop_test.go`, `maturity_touch_test.go`) — E5.
- Modify `backend/internal/pool/route_smart.go` — E6: hapus scorer/stick/overflow (pertahankan FIFO slot sementara; pindah di I1).
- Modify `backend/internal/pool/acquire_order.go` — E6: strip ke order index polos (diganti total di C1).
- Modify `backend/internal/pool/acquire_route.go` — E6: hapus leader-election gate (bioarkan single-flight session manager per token).
- Modify `backend/internal/server/engine_attempt.go` — E7: hapus retry-once (satu attempt, surface).
- Delete `backend/internal/session/session_park.go` — E8: hapus park-vs-drop.
- Modify `backend/internal/pool/pool_lifecycle.go` + `backend/internal/pool/lifecycle.go` — E8: hapus idle-end sweep + poll-drop; hardcode pacing poll.

### Fase I — Implement (bangun mekanisme baru)

- Create `backend/internal/pool/slot_ledger.go` — I1: ledger slot FIFO per (akun, model), keyed `map[token]map[model]slotState` atau setara (pindahan `routeSlotState`/`routeSlotPermit`/`routeSlotAcquire` dari `route_smart.go:151-277`, di-rename + di-key per model).
- Modify `backend/internal/config/config.go` + `config_keys.go` + `config_load.go` — I1: `SLOTS_PER_ACCOUNT`; I4: `PIN_MODEL`; I2: `MAX_SPILL_ACCOUNTS`.
- Create `backend/internal/pool/spill_queue.go` — I2: ordered lanes per model + spill-on-wait-expired + 429-requeue `notBefore`.
- Create `backend/internal/pool/precious.go` — I3: precious open-set {(token, model)} + guard anti-drop.
- Create `backend/internal/config/pin_model.go`, delete `backend/internal/pool/model_locks.go` (+ `model_locks_test.go`) — I4: parse/validasi strict `PIN_MODEL`, hapus `MODEL_LOCKS`.
- Modify `backend/internal/pool/acquire_route.go` — I5: admission/run-start 429 quota → requeue lane yang sama (tanpa cooldown write, tanpa failover).

### Fase C — Cutover (kunci order + knobs + UI)

- Create `backend/internal/pool/spill_order.go`, delete `backend/internal/pool/acquire_order.go` (+ `acquire_order_test.go`) — C1: order ketat index #1→#N.
- Modify `backend/internal/config/keycatalog.go` + `data.go` — C2: katalog knobs final, hapus entri mati.
- Modify `frontend/src/lib/utils/poolStrategy.js` + `StrategyPresetCard.svelte` + `TrafficSettings.svelte` + `AdvancedSettings.svelte` — C2: preset + copy mengikuti knobs final.
- Modify `frontend/src/lib/components/TokenDetailsDrawer.svelte` — C3: editor PIN tunggal per akun.
- Modify `backend/internal/pool/concurrency_ladder_test.go` + `queue_wait_test.go` — C4: ekspektasi lama (overflow-assist, rotasi) diganti ekspektasi spill; keeper repro R1–R6 dipromosi.

---

### Task E1: Stop failover walk pada refusal korelatif

**Files:**
- Modify: `backend/internal/pool/acquire_route.go:435-480` (cabang admission-error: `c.ipCapped`, `c.limitedIp`, `c.countryBlocked`)
- Modify: `backend/internal/pool/acquire_route.go:538-559` (cabang run-start-error yang sama)
- Test: `backend/internal/pool/spill_excise_correlative_test.go` (throwaway, hapus setelah hijau kecuali dipromosi di C4)

**Interfaces:**
- Consumes: `p.classifyAndCooldown(runsMgr, err) *classifiedError` (`backend/internal/pool/cooldown.go:267`) dengan field `ipCapped *upstream.IpCappedError`, `limitedIp *upstream.LimitedIpError`, `countryBlocked *upstream.CountryBlockedError`; `testutil.MockUpstream{SessionHandler, RequestsSnapshot}` (`backend/internal/testutil/mockupstream.go:115,686`).
- Produces: invarian untuk E2–E5: "setelah classify, refusal korelatif tidak pernah `continue` ke token berikut".

- [ ] **Step 1: tulis throwaway test korelatif-tidak-walk**

```go
func TestExciseCorrelativeNoWalk(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"ip_capped","retryAfterMs":60000}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if err == nil {
		t.Fatal("want ip_capped surface, got lease")
	}
	if !errors.Is(err, upstream.ErrIpCapped) {
		t.Fatalf("want ErrIpCapped, got %v", err)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("walked to account #2 (%d requests), want 0", n)
	}
}
```

- [ ] **Step 2: jalankan, harapkan GAGAL** (hari ini walk: mock1 tersentuh + tiap akun ter-cooldown)

Run: `go test ./backend/internal/pool/ -run TestExciseCorrelativeNoWalk -count=1`
Expected: FAIL di `walked to account #2`.

- [ ] **Step 3: potong walk — surface langsung tanpa `continue`**

Di `acquire_route.go:435-456` (admission) ganti ketiga cabang bucket korelatif menjadi surface langsung; contoh untuk ip_capped (ulangi pola sama untuk `limitedIp` dan `countryBlocked`, dan untuk blok run-start di `:538-559`):

```go
if ice := c.ipCapped; ice != nil {
    routeSlot.Release()
    return nil, ice
}
```

Hapus `appendIpCapped`/`appendCountryBlock`/`MarkModelUnfit` di jalur ini (unfit dihapus total di E3; `MarkModelUnfit` di `:469` ikut terhapus di sini dan registrarnya di E3). `banned` TETAP masuk bucket+quarantine (keeper, lihat E4).

- [ ] **Step 4: jalankan, harapkan PASS**

Run: `go test ./backend/internal/pool/ -run TestExciseCorrelativeNoWalk -count=1`
Expected: PASS; `mock1.RequestsSnapshot()==0`.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/acquire_route.go backend/internal/pool/spill_excise_correlative_test.go
git commit -m "feat(pool): surface correlative refusals without failover walk"
```

### Task E2: Hapus cooldown per-token untuk ip_capped

**Files:**
- Modify: `backend/internal/pool/cooldown.go:39-51` (hapus `CooldownTokenIpCapped`)
- Modify: `backend/internal/pool/cooldown.go:267-398` (hapus cabang ip_capped + jitter + readmit-cap di `classifyAndCooldown`)
- Modify: `backend/internal/runs/cooldown.go:17-36` (hapus sisi runs: `CooldownIpCapped`, `IPMaxReadmits`, jitter)
- Modify: `backend/internal/config/cooldown.go:146-165` (hapus `IpMaxReadmits`, `IpJitterRatio` + konstanta)
- Modify: `backend/internal/server/engine_attempt.go:379-391` (hapus `CooldownIpCapped` chat-path; ganti surface langsung)
- Test: perbarui `spill_excise_correlative_test.go` (tambah subtest cooldown-tidak-tertulis)

**Interfaces:**
- Consumes: invarian E1 (ip_capped surface langsung, tidak `continue`).
- Produces: `runs.RunManager` tanpa metode `CooldownIpCapped`; `config.Config` tanpa `IpMaxReadmits()/IpJitterRatio()` — E4 dan I5 mengasumsikan API ini sudah hilang.

- [ ] **Step 1: tulis subtest anti-cooldown**

```go
func TestExciseIpCappedWritesNoCooldown(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"ip_capped","retryAfterMs":60000}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.Acquire(ctx, modelA)
	tok := (*p.roster.Load())[0]
	if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
		t.Fatalf("ip_capped wrote cooldown until %s, want none", until.Format(time.RFC3339))
	}
	if ice := tok.runs.IpCappedError(); ice != nil {
		t.Fatalf("ip_capped remembered %v, want nil", ice)
	}
}
```

- [ ] **Step 2: jalankan, harapkan GAGAL** (cooldown tertulis + jitter + readmit counter)

Run: `go test ./backend/internal/pool/ -run TestExciseIpCappedWritesNoCooldown -count=1`
Expected: FAIL di `wrote cooldown`.

- [ ] **Step 3: hapus implementasi**

Hapus `CooldownTokenIpCapped` (`pool/cooldown.go:45-51`), cabang ip_capped di `classifyAndCooldown` (`:267-398`, termasuk jitter ±20% dan kunci harian ke-3), metode runs + konstanta `IPMaxReadmits`/jitter (`runs/cooldown.go`), accessor config (`config/cooldown.go:151-165`). Di `engine_attempt.go:379-391` ganti isi cabang menjadi:

```go
case errors.Is(err, upstream.ErrIpCapped):
    release()
    return nil, nil, err
```

- [ ] **Step 4: jalankan, harapkan PASS** + hapus referensi sisa via compiler

Run: `go test ./backend/internal/pool/ -run 'TestExciseCorrelativeNoWalk|TestExciseIpCappedWritesNoCooldown' -count=1`
Expected: PASS. Referensi `CooldownIpCapped`/`IpMaxReadmits` yang tersisa (test lama) dihapus di task ini juga — compiler adalah daftarnya.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/cooldown.go backend/internal/runs/cooldown.go backend/internal/config/cooldown.go backend/internal/server/engine_attempt.go backend/internal/pool/spill_excise_correlative_test.go
git commit -m "feat(pool): drop per-token ip_capped cooldown, surface natural 429"
```

### Task E3: Hapus registry unfit global 5m

**Files:**
- Delete: `backend/internal/pool/unfit.go`, `backend/internal/pool/unfit_test.go`
- Modify: `backend/internal/pool/acquire_route.go:458-474` (sisa cabang `c.limitedIp` yang belum dipotong E1 — surface langsung)
- Modify: `backend/internal/server/engine_attempt.go:220-229,254-271` (hapus `MarkModelUnfit` + `ClearModelUnfitBefore` di success dan cabang limited_ip)
- Test: `spill_excise_correlative_test.go` (subtest limited_ip tidak walk + tidak ada mark)

**Interfaces:**
- Consumes: invarian E1.
- Produces: tidak ada simbol `MarkModelUnfit`/`ModelUnfit`/`ClearModelUnfit*` di repo — I5 dan C1 mengasumsikan registry ini tidak ada.

- [ ] **Step 1: tulis subtest limited_ip**

```go
func TestExciseLimitedIPNoWalkNoMark(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"session_model_mismatch","message":"model limited on this egress ip"}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if err == nil || !errors.Is(err, upstream.ErrModelIPLimited) {
		t.Fatalf("want ErrModelIPLimited, got %v", err)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("walked to account #2 (%d requests), want 0", n)
	}
}
```

Body memakai marker `limited` agar klasifikasi `classify.go:143-151` menghasilkan `ErrModelIPLimited`; bila mock menskrip via `SessionSequence`, ganti body dengan mode sequence yang ekuivalen dan pertahankan assertion yang sama.

- [ ] **Step 2: jalankan, harapkan GAGAL** (walk dan/atau unfit mark 5m)

Run: `go test ./backend/internal/pool/ -run TestExciseLimitedIPNoWalkNoMark -count=1`
Expected: FAIL.

- [ ] **Step 3: hapus registry + call sites**

Hapus file `unfit.go`/`unfit_test.go`. Di `acquire_route.go:458-474` ganti cabang `c.limitedIp` menjadi:

```go
if lie := c.limitedIp; lie != nil {
    lie.Model = model
    errs = append(errs, fmt.Sprintf("%s: %v", name, err))
    routeSlot.Release()
    return nil, err
}
```

Di `engine_attempt.go` hapus blok `ClearModelUnfitBefore` (`:227-229`) dan seluruh cabang `ErrModelIPLimited` (`:254-271`) menjadi:

```go
case errors.Is(err, upstream.ErrModelIPLimited):
    release()
    return nil, nil, err
```

- [ ] **Step 4: jalankan, harapkan PASS**

Run: `go test ./backend/internal/pool/ -run 'TestExciseLimitedIPNoWalkNoMark|TestExciseCorrelativeNoWalk' -count=1`
Expected: PASS. `grep -r MarkModelUnfit backend/` harus kosong.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/acquire_route.go backend/internal/server/engine_attempt.go backend/internal/pool/spill_excise_correlative_test.go
git rm -q backend/internal/pool/unfit.go backend/internal/pool/unfit_test.go
git commit -m "feat(pool): delete global model-unfit registry, surface limited_ip"
```

### Task E4: Hapus COOLDOWN_* bounded + ceiling (passthrough RetryAfter); pertahankan ban quarantine

**Files:**
- Modify: `backend/internal/upstream/classify.go:260-308` (hapus konstanta Fanout/InvalidModel/LoadShed/PeakHours/Opaque + pemakaian; RetryAfter upstream diteruskan apa adanya)
- Modify: `backend/internal/config/cooldown.go` (hapus total file bila hanya tersisa accessor mati; bila `DefaultMs`/`CountryBlockMs` masih dipakai, hapus pemakainya dulu di task ini)
- Modify: `backend/internal/runs/cooldown.go:17-36` (hapus `DefaultCooldown`, countryBlock, ceiling di sisi runs)
- Modify: `backend/internal/pool/cooldown.go:267-398` (sederhanakan `classifyAndCooldown`: 401/ban/rate-limit passthrough; country-block → surface seperti korelatif E1)
- Modify: `backend/internal/server/engine_attempt.go:345-355,392-401` (auth + country: surface tanpa cooldown write)
- Test: `spill_excise_cooldown_test.go` (throwaway: 429 opaque tanpa RetryAfter tidak menulis cooldown; ban tetap quarantine)

**Interfaces:**
- Consumes: invarian E1–E3.
- Produces: `classifyAndCooldown` tanpa field `authRejected`-cooldown (401 = surface), tanpa penulisan cooldown untuk 429 non-kuota; satu-satunya state per-akun yang tersisa: quarantine ban/suspend + slot ledger + precious set. I5 mengasumsikan tidak ada `CooldownRateLimit` untuk 429 kuota.

- [ ] **Step 1: tulis throwaway test passthrough**

```go
func TestExciseOpaque429WritesNoCooldown(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"some_future_code"}`))
	}
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.Acquire(ctx, modelA)
	tok := (*p.roster.Load())[0]
	if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
		t.Fatalf("opaque 429 wrote cooldown until %s, want passthrough", until.Format(time.RFC3339))
	}
}

func TestExciseBanStillQuarantined(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock0.SetBan(true)
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if err == nil || !errors.Is(err, upstream.ErrBanned) {
		t.Fatalf("want ErrBanned, got %v", err)
	}
	if q := (*p.roster.Load())[0].quarantine.Load(); q == nil {
		t.Fatal("ban did not quarantine, want terminal quarantine")
	}
}
```

- [ ] **Step 2: jalankan, harapkan GAGAL parsial** (opaque menulis cooldown 60s via `COOLDOWN_OPAQUE_MS`)

Run: `go test ./backend/internal/pool/ -run 'TestExciseOpaque429WritesNoCooldown|TestExciseBanStillQuarantined' -count=1`
Expected: FAIL di opaque; ban PASS (keeper).

- [ ] **Step 3: hapus bounded backoff, pertahankan ban**

Hapus konstanta bounded di `classify.go:260-308` (Fanout 60s, InvalidModel 60s, Opaque 60s, LoadShed 90s, PeakHours 30m) dan clamp ceiling 7d: RetryAfter/ResetAt upstream diteruskan tanpa clamp. Hapus `config/cooldown.go` total + pemakainya, `runs/cooldown.go:17-36` (kecuali `CooldownBan` + quarantine path `pool/cooldown.go:56-71,400-449`). 401 (`engine_attempt.go:345-348`, `acquire_route.go:407-413`) dan country-block (`:392-401`, `pool/cooldown.go:78-84`) menjadi surface-tanpa-cooldown seperti E1. Quarantine ban/suspend + `clearLiftedQuarantine` DIPERTAHANKAN utuh.

- [ ] **Step 4: jalankan, harapkan PASS**

Run: `go test ./backend/internal/pool/ -run 'TestExciseOpaque429WritesNoCooldown|TestExciseBanStillQuarantined' -count=1`
Expected: PASS. `grep -rn COOLDOWN_ backend/internal --include='*.go' | grep -v _test` harus kosong (test lama yang gagal kompilasi dimigrasi di task ini).

- [ ] **Step 5: commit**

```bash
git add backend/internal/upstream/classify.go backend/internal/pool/cooldown.go backend/internal/runs/cooldown.go backend/internal/server/engine_attempt.go backend/internal/pool/spill_excise_cooldown_test.go
git rm -q backend/internal/config/cooldown.go
git commit -m "feat(pool): delete bounded cooldowns, passthrough upstream RetryAfter, keep ban quarantine"
```

### Task E5: Hapus probe/maturity proaktif

**Files:**
- Delete: `backend/internal/pool/quota_smartprobe.go` (+ 4 test), `quota_visitprobe.go` (+ test), `quota_bootseed.go` (+ test), `maturity.go` (+ 7 test)
- Modify: `backend/internal/pool/pool.go` (hapus wiring scheduler: fields `probeCtx`, `maintain`, pemicu `StreakHits`/probe; pertahankan `wg sync.WaitGroup` untuk shutdown lain bila masih dipakai)
- Modify: `backend/internal/config/config_keys.go:196-200` + `keycatalog.go` + `frontend` copy (hapus `MATURITY_*`, `QUOTA_PROBE_*`, `MATURITY_TOUCH_MODEL`, `QuotaAutoProbe`)
- Test: tidak ada throwaway perilaku; verifikasi = `SessionProbesSnapshot()==0` + `StreakHitsSnapshot()==0` pasca traffic (tambah ke `spill_excise_cooldown_test.go` sebagai subtest, keeper di C4)

**Interfaces:**
- Consumes: tidak ada (independen dari E1–E4; boleh paralel).
- Produces: nol kontak upstream di luar traffic + re-admit on-demand; `testutil.MockUpstream.SessionProbesSnapshot/StreakHitsSnapshot` (`mockupstream.go:745,760`) menjadi oracle nol-probe untuk semua task.

- [ ] **Step 1: tulis subtest nol-probe**

```go
func TestExciseNoProactiveContact(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.LeaseRelease(l)
	if n := mock0.SessionProbesSnapshot(); n != 0 {
		t.Fatalf("%d token-level probes, want 0", n)
	}
	if n := mock0.StreakHitsSnapshot(); n != 0 {
		t.Fatalf("%d streak hits, want 0", n)
	}
}
```

- [ ] **Step 2: jalankan, harapkan GAGAL** (probe/maturity menyentuh mock)

Run: `go test ./backend/internal/pool/ -run TestExciseNoProactiveContact -count=1`
Expected: FAIL (probe dan/atau streak > 0).

- [ ] **Step 3: hapus scheduler + file**

Hapus 4 file sumber + 13 file test di atas. Cabut wiring di `pool.go` (maintain goroutine, probeCtx, trigger streak). Hapus knobs `MATURITY_ENABLED`, `MATURITY_BACKOFF_MS`, `MATURITY_TOUCH_MODEL`, `QUOTA_AUTO_PROBE`, `QUOTA_PROBE_ACTIVE_INTERVAL`, `QUOTA_PROBE_IDLE_HEARTBEAT`, `SMART_PROBE_BACKOFF_MAX_MS` dari `config_keys.go` + keycatalog + UI copy.

- [ ] **Step 4: jalankan, harapkan PASS**

Run: `go test ./backend/internal/pool/ -run TestExciseNoProactiveContact -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/pool.go backend/internal/config/config_keys.go backend/internal/pool/spill_excise_cooldown_test.go
git rm -q backend/internal/pool/quota_smartprobe.go backend/internal/pool/quota_smartprobe_test.go backend/internal/pool/quota_smartprobe_fleet_test.go backend/internal/pool/quota_autoprobe_test.go backend/internal/pool/smartprobe_persist_test.go backend/internal/pool/quota_visitprobe.go backend/internal/pool/quota_visitprobe_test.go backend/internal/pool/quota_bootseed.go backend/internal/pool/quota_bootseed_test.go backend/internal/pool/maturity.go backend/internal/pool/maturity_test.go backend/internal/pool/maturity_auto_test.go backend/internal/pool/maturity_firegate_test.go backend/internal/pool/maturity_persist_test.go backend/internal/pool/maturity_resultday_test.go backend/internal/pool/maturity_skip_loop_test.go backend/internal/pool/maturity_touch_test.go
git commit -m "feat(pool): delete proactive probes and maturity touches"
```

### Task E6: Hapus scorer/rotasi/leader-election (order index sementara)

**Files:**
- Modify: `backend/internal/pool/route_smart.go:56-85,333-365,389-569` (hapus weights, transient penalty, stick/overflow helpers, `routeScore`, `routeSmartRank`; PERTAHANKAN `routeSlotState`/`routeSlotPermit`/`routeSlotAcquire`/`routeSlotParams` untuk I1)
- Modify: `backend/internal/pool/acquire_order.go:60-205` (ganti tier+rotasi+demote dengan order index polos)
- Modify: `backend/internal/pool/acquire_route.go:56-121` (hapus leader gate + `rr` start; `start` selalu 0)
- Delete: `backend/internal/pool/admission_leader_election_test.go`, `route_smart_test.go` (ganti keeper slot di I1), `acquire_order_test.go`, `random_rotation_test.go`
- Test: throwaway `spill_order_strict_test.go`: burst dingin konkuren → semua lease `Token==0` dulu; `SessionCreatesSnapshot` mock0 == 1 (single-flight session manager, bukan gate)

**Interfaces:**
- Consumes: invarian E1 (tidak ada walk korelatif).
- Produces: `acquireOrder` sementara ber flowing order index polos `[0..N)`; `routeSmartRank`/`routeScore`/`routeOverflowHelper`/`routeStickHolders` hilang — C1 mengganti `acquireOrder` dengan `spill_order.go` final.

- [ ] **Step 1: tulis throwaway order-index + single-admit**

```go
func TestExcisePlainIndexOrder(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.TokenMaxConcurrent = 8
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	leases := make([]*Lease, 4)
	errs := make([]error, 4)
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = p.Acquire(ctx, modelA)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if leases[i].Token != 0 {
			t.Fatalf("worker %d leased account #%d, want #1 (index order)", i, leases[i].Token+1)
		}
		defer p.LeaseRelease(leases[i])
	}
	if n := mock0.SessionCreatesSnapshot(); n != 1 {
		t.Fatalf("account #1 admitted %d sessions, want 1 (no duplicate creates)", n)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("account #2 touched (%d requests), want 0", n)
	}
}
```

- [ ] **Step 2: jalankan, harapkan GAGAL** (rr-start/lastUsed/scorer menyebar ke akun #2)

Run: `go test ./backend/internal/pool/ -run TestExcisePlainIndexOrder -count=1`
Expected: FAIL di `leased account #2`.

- [ ] **Step 3: strip ke order index**

`acquire_order.go`: hapus `lastTokenByModel`, `admissions`, tier hot/cold/mismatch, sort Freebucks, switch `TOKEN_ROTATION`, demote availability; kembalikan `[0..N)` disaring `locked/quarantine/lockedOutByModel/capped` saja. `acquire_route.go`: hapus gate `modelAdmissionGate` + `p.rr.Add(1)` (start=0). `route_smart.go`: hapus scorer/stick/overflow/rank, pertahankan blok slot FIFO `:99-322`.

- [ ] **Step 4: jalankan, harapkan PASS**

Run: `go test ./backend/internal/pool/ -run TestExcisePlainIndexOrder -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/route_smart.go backend/internal/pool/acquire_order.go backend/internal/pool/acquire_route.go backend/internal/pool/spill_order_strict_test.go
git rm -q backend/internal/pool/admission_leader_election_test.go backend/internal/pool/route_smart_test.go backend/internal/pool/acquire_order_test.go backend/internal/pool/random_rotation_test.go
git commit -m "feat(pool): strip scorer, rotation and leader gate to plain index order"
```

### Task E7: Hapus retry-once chat (satu attempt, surface)

**Files:**
- Modify: `backend/internal/server/engine_attempt.go:139-479` (hapus loop re-acquire; tiap cabang error = `release()` + surface; hapus `transientErr`, `attempts>1` budget)
- Modify: `backend/internal/config/config.go:91` + `config_keys.go` + keycatalog + UI (hapus `RATE_LIMIT_FAILOVER`)
- Test: throwaway `spill_excise_nochatretry_test.go` di `backend/internal/server/`: chat 429 sekali → `SessionCreatesSnapshot` tidak bertambah (tanpa re-admit kedua)

**Interfaces:**
- Consumes: `backend.CooldownRateLimit/CooldownIpCapped/CooldownBan` yang sudah dirampingkan E2/E4 (panggil yang tersisa saja; cabang 429 TIDAK menulis cooldown setelah E4/I5).
- Produces: `chatCore` single-attempt; I5 + C1 mengasumsikan tidak ada re-acquire di chat path.

- [ ] **Step 1: tulis throwaway anti-retry**

```go
func TestExciseChatSingleAttempt(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.ChatStatus = 429
	mock0.ChatErrorBody = `{"error":"rate_limited","retryAfterMs":60000}`
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0, mock1)
	before := mock0.SessionCreatesSnapshot() + mock1.SessionCreatesSnapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	l, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer p.LeaseRelease(l)
	_ = doChatOnce(ctx, p, l, modelA)
	after := mock0.SessionCreatesSnapshot() + mock1.SessionCreatesSnapshot()
	if after != before {
		t.Fatalf("chat refusal re-admitted %d sessions, want 0 (single attempt)", after-before)
	}
}
```

`doChatOnce` adalah helper test yang memanggil satu `backend.Chat` tanpa loop (tulis inline di file test, 10 baris, memakai konstruktor engine yang sudah dipakai test server yang ada — JANGAN tiru pola `attempts`; satu panggilan langsung).

- [ ] **Step 2: jalankan, harapkan GAGAL** (retry-once menambah 1 admission di token lain)

Run: `go test ./backend/internal/server/ -run TestExciseChatSingleAttempt -count=1`
Expected: FAIL di `re-admitted 1`.

- [ ] **Step 3: potong loop**

Ubah `for {` di `engine_attempt.go:213` menjadi satu panggilan `backend.Chat`; tiap cabang `case` di `:253-429` berakhir `release(); return nil, nil, err`; hapus blok re-acquire `:430-478` + `transientErr`. Hapus knob `RATE_LIMIT_FAILOVER` di semua lapisan.

- [ ] **Step 4: jalankan, harapkan PASS**

Run: `go test ./backend/internal/server/ -run TestExciseChatSingleAttempt -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/server/engine_attempt.go backend/internal/config/config.go backend/internal/config/config_keys.go backend/internal/server/spill_excise_nochatretry_test.go
git commit -m "feat(server): single-attempt chat, delete retry-once and RATE_LIMIT_FAILOVER"
```

### Task E8: Hapus park/sweep (session hidup sampai upstream mengakhirinya)

**Files:**
- Delete: `backend/internal/session/session_park.go`
- Modify: `backend/internal/pool/pool_lifecycle.go:237-265` (hapus `SESSION_IDLE_END` sweep), `:40-49` (hardcode pacing poll 20s base / 5m cap)
- Modify: `backend/internal/pool/lifecycle.go:119-203` (hapus jalur drop-on-cooldown; `Invalidate*` hanya untuk mayat upstream: invalid/expired/superseded/428-required)
- Modify: `backend/internal/pool/cooldown.go:54-68` (`SessionParkThresholdMs` wiring di `cooldown_tuning.go` — hapus file `cooldown_tuning.go` + test)
- Modify: config + keycatalog + UI (hapus `SESSION_PARK_ENABLED`, `SESSION_PARK_THRESHOLD_MS`, `SESSION_POLL_MAX_MS`, `SESSION_IDLE_END`)
- Test: throwaway: cooldown pendek tidak meng-drop session (`Snapshot().Usable()` tetap true; `SessionEndsSnapshot()==0`)

**Interfaces:**
- Consumes: tidak ada (paralel dengan E1–E7).
- Produces: invarian precious-sementara "tidak ada drop lokal"; I3 membangun `precious.go` di atasnya.

- [ ] **Step 1: tulis throwaway anti-drop**

```go
func TestExciseShortCooldownKeepsSession(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.LeaseRelease(l)
	tok := (*p.roster.Load())[0]
	tok.runs.Cooldown(5 * time.Minute)
	p.MaintainOnce(ctx)
	if n := mock0.SessionEndsSnapshot(); n != 0 {
		t.Fatalf("short cooldown ended %d sessions, want 0", n)
	}
	if !tok.session.Snapshot().Usable() {
		t.Fatal("session not usable after short cooldown, want usable")
	}
}
```

`MaintainOnce` adalah satu tick maintain sinkron yang sudah ada (bila namanya berbeda di `pool_lifecycle.go`, pakai nama aktualnya — JANGAN buat helper baru).

- [ ] **Step 2: jalankan, harapkan GAGAL** (park-threshold/drop mengakhiri session)

Run: `go test ./backend/internal/pool/ -run TestExciseShortCooldownKeepsSession -count=1`
Expected: FAIL.

- [ ] **Step 3: hapus park/sweep/drop**

Hapus `session_park.go`, `cooldown_tuning.go`(+test), cabang `shouldPark`/drop di `lifecycle.go:119-203`, sweep `SESSION_IDLE_END` (`pool_lifecycle.go:237-265`), hardcode pacing poll, hapus 4 knob di semua lapisan.

- [ ] **Step 4: jalankan, harapkan PASS**

Run: `go test ./backend/internal/pool/ -run TestExciseShortCooldownKeepsSession -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/pool_lifecycle.go backend/internal/pool/lifecycle.go backend/internal/pool/spill_excise_park_test.go
git rm -q backend/internal/session/session_park.go backend/internal/pool/cooldown_tuning.go backend/internal/pool/cooldown_tuning_test.go backend/internal/pool/cooldown_session_survive_test.go
git commit -m "feat(pool): delete park-vs-drop and idle sweep, sessions survive cooldowns"
```

---

### Task I1: Slot ledger 2 per (akun, model) (R2)

**Files:**
- Create: `backend/internal/pool/slot_ledger.go` (pindahan `routeSlotState`, `routeSlotWaiter`, `routeSlotPermit`, `routeSlotAcquire`, `routeSlotLive`, `routeSlotQueued`, `routeSlotStats` dari `route_smart.go:99-322`, di-rename `slot*`; ledger keyed per (token, model) — `map[token]map[model]slotState` atau setara, FIFO per (akun, model); `routeSlotParams` → `slotParams` membaca `SLOTS_PER_ACCOUNT` sebagai cap per (akun, model))
- Modify: `backend/internal/pool/route_smart.go` (hapus blok pindahan; file ini akan dihapus total di C1 bila tidak tersisa — catat)
- Modify: `backend/internal/config/config.go` + `config_keys.go:202` + `config_load.go` + keycatalog + UI copy (rename `TOKEN_MAX_CONCURRENT` → `SLOTS_PER_ACCOUNT`, default 2, floor 1; `0` = unlimited seperti `route_smart.go:124-137` hari ini)
- Modify: `backend/internal/pool/acquire_route.go:295-355` (pakai `slotAcquire`; hapus overflow-assist `:304-336`, pertahankan mapping queue-exhausted untuk I2)
- Test: keeper `backend/internal/pool/slot_ledger_test.go`: cap 2 per (akun, model) → lease ke-3 model SAMA parkir (`QueueWait>0` setelah release) + sub-kasus isolasi model (2×modelA + 2×modelB di akun #1 = 4 jalan, akun #2 nol kontak), `routeQueueHint`/`Retry-After 1s` DIHAPUS — waiter tidak menerima 429 lokal (lihat I2)

**Interfaces:**
- Consumes: `config.Config.SlotsPerAccount int` (cap per (akun, model)), `QueueDepth int`, `QueueWait time.Duration` (resolved, zero-tolerant seperti loader hari ini).
- Produces:
```go
func (p *Pool) slotAcquire(ctx context.Context, key slotKey, displayIdx int, cap, depth int, wait time.Duration) (*slotPermit, bool, error)
func (s *slotPermit) Release()
func (p *Pool) slotLive(key slotKey) int
type slotQueueExhaustedError struct { Reason string; Token, Cap, Live int; Wait time.Duration }
```
`slotKey` mencakup (token, model) — slot dan FIFO di-key per (akun, model).
I2 memakai `slotAcquire` + `slotQueueExhaustedError` untuk spill; C1 menghapus sisa `route_smart.go`.

- [ ] **Step 1: tulis keeper cap-2-parkir + isolasi model (R2)**

```go
func TestSlotLedgerTwoSlotsThirdParks(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.TokenMaxConcurrent = 2
		c.QueueWait = 2 * time.Second
		c.QueueDepth = 16
	}, mock0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("lease 1: %v", err)
	}
	defer p.LeaseRelease(m1)
	m2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("lease 2: %v", err)
	}
	defer p.LeaseRelease(m2)
	done := make(chan *Lease, 1)
	go func() {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			return
		}
		done <- l
	}()
	select {
	case <-done:
		t.Fatal("lease 3 granted while 2 slots held, want parked")
	case <-time.After(300 * time.Millisecond):
	}
	p.LeaseRelease(m1)
	select {
	case l3 := <-done:
		if l3.QueueWait <= 0 {
			t.Fatalf("lease 3 QueueWait=%v, want >0 (parked FIFO)", l3.QueueWait)
		}
		if l3.Token != 0 {
			t.Fatalf("lease 3 on account #%d, want #1", l3.Token+1)
		}
		p.LeaseRelease(l3)
	case <-time.After(5 * time.Second):
		t.Fatal("lease 3 never granted after slot freed")
	}
}
```

Sub-kasus isolasi model (slot model lain tidak termakan):

```go
func TestSlotLedgerPerModelIsolation(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 2 * time.Second
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var held []*Lease
	defer func() {
		for _, l := range held {
			p.LeaseRelease(l)
		}
	}()
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("modelA lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(ctx, modelB)
		if err != nil {
			t.Fatalf("modelB lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	for i, l := range held {
		if l.Token != 0 {
			t.Fatalf("lease %d on account #%d, want #1 (2xA + 2xB share one account)", i, l.Token+1)
		}
		if l.QueueWait != 0 {
			t.Fatalf("lease %d parked (%v), want zero parks (per-model slots free)", i, l.QueueWait)
		}
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("account #2 touched (%d requests), want 0", n)
	}

(Nama field config `TokenMaxConcurrent`/`QueueWait`/`QueueDepth` diganti `SlotsPerAccount`/dst. pada step rename di task ini juga.)

- [ ] **Step 2: jalankan, harapkan PASS setelah pindahan** (perilaku cap-2 sudah ada; isolasi model FAIL sampai ledger di-key per (token, model); task ini rename + isolasi file)

Run: `go test ./backend/internal/pool/ -run 'TestSlotLedgerTwoSlotsThirdParks|TestSlotLedgerPerModelIsolation' -count=1`
Expected: PASS pasca pindah (FAIL kompilasi sebelum rename field — itu daftar rename; FAIL isolasi sebelum keying per model — itu daftar keying).

- [ ] **Step 3: pindah + rename + key per model**

Pindah blok FIFO ke `slot_ledger.go` dengan rename di atas, keyed `map[token]map[model]slotState` atau setara (FIFO per (akun, model)); rename knob di config/load/keycatalog/UI copy/`poolStrategy` tidak (bukan strategy key); `acquire_route.go` pakai `slotAcquire` keyed (token, model), hapus overflow-assist.

- [ ] **Step 4: jalankan keeper + reproduksi R2 angka**

Run: `go test ./backend/internal/pool/ -run 'TestSlotLedgerTwoSlotsThirdParks|TestSlotLedgerPerModelIsolation' -count=1 -v`
Expected: PASS. **Repro R2 (3-req-1-akun-model-sama):** 3 request model SAMA konkuren ke 1 akun (cap 2 per (akun, model)) → 2 lease `Token==0, QueueWait==0` + 1 parkir lalu `QueueWait>0` setelah slot dibebaskan; `SessionCreatesSnapshot()` mock0 == 1 (satu precious session dipakai bersama, bukan 3 admission). **Isolasi model:** 2×modelA + 2×modelB di akun #1 = 4 jalan tanpa antre/spill, akun #2 nol kontak.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/slot_ledger.go backend/internal/pool/slot_ledger_test.go backend/internal/pool/route_smart.go backend/internal/pool/acquire_route.go backend/internal/config/
git commit -m "feat(pool): slot ledger SLOTS_PER_ACCOUNT=2 per account-model"
```

### Task I2: Ordered lanes + spill-on-wait-expired (R3)

**Files:**
- Create: `backend/internal/pool/spill_queue.go` (lane per model: `lanes[model] = [akun...]` dalam index order tersaring pin/liveness; waiter parkir di lane head; total parkir > QUEUE_WAIT → pindah ke lane next (`spillCount++`); `MAX_SPILL_ACCOUNTS>0` membatasi; tak ada lane next → surface `slotQueueExhaustedError` timeout)
- Modify: `backend/internal/pool/acquire_route.go:149-355` (`leaseFromOrder` menjadi driver lane: coba lane head → `slotAcquire` → timeout/full → lane next)
- Modify: `backend/internal/config/*` (tambah `MAX_SPILL_ACCOUNTS`, default 0 = tanpa batas)
- Test: keeper `spill_queue_test.go`: burst-5-2-akun (di bawah)

**Interfaces:**
- Consumes: `slotAcquire/slotPermit/slotQueueExhaustedError` (I1); `spillOrder(model) []int` sementara = order index E6 (final di C1); `config.MaxSpillAccounts int`, `config.QueueWait time.Duration`.
- Produces:
```go
func (p *Pool) spillLane(model string) []int
func (p *Pool) acquireSpill(ctx context.Context, model, agentID string, cfg *config.Config, toks *[]*tokenEntry) (*Lease, error)
```
C1 memakai `spillLane`; I5 memakai `acquireSpill` sebagai titik requeue.

- [ ] **Step 1: tulis keeper burst-5-2-akun (R3)**

```go
func TestSpillBurst5TwoAccounts(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.ChatBlocks = true
	mock1.ChatBlocks = true
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 300 * time.Millisecond
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	leases := make([]*Lease, 5)
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := p.Acquire(ctx, modelA)
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			leases[i] = l
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, l := range leases {
			p.LeaseRelease(l)
		}
	}()
	on0, on1 := 0, 0
	for i, l := range leases {
		if l == nil {
			t.Fatalf("worker %d: no lease", i)
		}
		switch l.Token {
		case 0:
			on0++
		case 1:
			on1++
		default:
			t.Fatalf("worker %d on account #%d, want #1/#2", i, l.Token+1)
		}
	}
	if on0 != 2 || on1 != 2 {
		t.Fatalf("split #%d/#%d, want 2/2 with 1 queued", on0, on1)
	}
	queued := 0
	for _, l := range leases {
		if l.QueueWait > 0 {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("%d parked leases, want 1 (2+2 run + 1 antre)", queued)
	}
}
```

(`ChatBlocks` menahan turn agar slot penuh saat burst tiba; bila field bernama lain di `mockupstream.go`, pakai nama aktualnya — field ini ada di `:105`.)

- [ ] **Step 2: jalankan, harapkan GAGAL** (tanpa spill: waiter ke-5 timeout-429 di akun #1, bukan grant di #2)

Run: `go test ./backend/internal/pool/ -run TestSpillBurst5TwoAccounts -count=1`
Expected: FAIL (`worker 4: pool: token-1 live-turn queue wait...`).

- [ ] **Step 3: implementasi lane + spill**

`spill_queue.go`: `spillLane` = index order tersaring (locked/quarantine/pin). `acquireSpill`: untuk tiap lane dalam order: `slotAcquire` dengan sisa `QUEUE_WAIT`; `slotQueueExhaustedError` timeout/full → lanjut lane next (`spillCount++`, cek `MAX_SPILL_ACCOUNTS`); grant → admission seperti `acquire_route.go:357-404` (tanpa walk: gagal admission non-kuota → surface; kuota → I5). Hapus mapping timeout→429-rate-limit + `continue` lama di `:320-341`.

- [ ] **Step 4: jalankan, harapkan PASS — repro R3**

Run: `go test ./backend/internal/pool/ -run TestSpillBurst5TwoAccounts -count=1 -v`
Expected: PASS. **Repro R3 (burst-5-2-akun):** 5 konkuren, 2 akun cap 2 → 2+2 jalan + 1 antre; akun #2 tersentuh HANYA setelah waiter kehabisan QUEUE_WAIT di #1 (buktikan: dengan `QUEUE_WAIT=30s` + turn instan, burst-5 selesai tanpa `mock1.SessionCreatesSnapshot()>0` bila slot #1 cukup — subtest kedua memakai wait pendek + ChatBlocks untuk memaksa spill; tulis keduanya).

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/spill_queue.go backend/internal/pool/spill_queue_test.go backend/internal/pool/acquire_route.go backend/internal/config/
git commit -m "feat(pool): ordered lanes with spill on queue-wait expiry"
```

### Task I3: Precious open-set (R4)

**Files:**
- Create: `backend/internal/pool/precious.go` (set `{(token, model)}` bersesi live; `preciousAdd/preciousHas/preciousRemove`; guard `mustKeepSession` dipakai semua jalur `Invalidate*/EndSession`)
- Modify: `backend/internal/pool/lifecycle.go:125-200` (`Invalidate*` menolak hapus precious yang `Usable()` kecuali mayat upstream: `superseded`, `session-invalid`, `expired`, `waiting_room_required`)
- Modify: `backend/internal/pool/pool.go` (snapshot: label precious per slot untuk dashboard)
- Test: keeper `precious_test.go`: N=2 akun × model sama → 2N=4 slot; isi 4 lease; tekanan (cooldown manual + maintain tick + request ke-5) → `SessionEndsSnapshot()==0` di kedua mock, 4 session `Usable()`

**Interfaces:**
- Consumes: invarian E8 (tidak ada drop lokal); `Lease{Token, Model}` (`pool.go:74-84`).
- Produces:
```go
func (p *Pool) preciousAdd(token int, model string)
func (p *Pool) preciousHas(token int, model string) bool
func (p *Pool) preciousRemove(token int, model string)
```
C1 memakai `preciousHas` untuk mempertahankan holder di head order; C3 menampilkan label.

- [ ] **Step 1: tulis keeper 2N slot precious (R4)**

```go
func TestPreciousTwoAccountsSameModel(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 5 * time.Second
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var held []*Lease
	for i := 0; i < 4; i++ {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	defer func() {
		for _, l := range held {
			p.LeaseRelease(l)
		}
	}()
	for i, tok := range *p.roster.Load() {
		tok.runs.Cooldown(5 * time.Minute)
		_ = i
	}
	p.MaintainOnce(ctx)
	done := make(chan error, 1)
	go func() {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			done <- err
			return
		}
		p.LeaseRelease(l)
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("request 5: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("request 5 never served")
	}
	if n := mock0.SessionEndsSnapshot() + mock1.SessionEndsSnapshot(); n != 0 {
		t.Fatalf("%d sessions ended under pressure, want 0 (precious)", n)
	}
	for i, tok := range *p.roster.Load() {
		if !tok.session.Snapshot().Usable() {
			t.Fatalf("account #%d session not usable, want precious-kept", i+1)
		}
	}
}
```

- [ ] **Step 2: jalankan, harapkan GAGAL** (cooldown manual + maintain mengakhiri/men-drop session)

Run: `go test ./backend/internal/pool/ -run TestPreciousTwoAccountsSameModel -count=1`
Expected: FAIL di `sessions ended` atau `not usable`.

- [ ] **Step 3: implementasi open-set + guard**

`precious.go` + guard di tiap `Invalidate*/EndSession` di `lifecycle.go`: tolak bila `preciousHas(token, model)` dan snapshot `Usable()` dan reason bukan mayat-upstream (`superseded` TETAP boleh drop: row dirampas instance lain — upstream yang mengakhiri, bukan kita; dokumentasikan di komentar).

- [ ] **Step 4: jalankan, harapkan PASS — repro R4**

Run: `go test ./backend/internal/pool/ -run TestPreciousTwoAccountsSameModel -count=1 -v`
Expected: PASS. **Repro R4:** N=2 akun × 1 model = 4 slot terisi + request ke-5 terlayani dari antrean; nol `SessionEnds`; kedua session tetap `Usable()`.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/precious.go backend/internal/pool/precious_test.go backend/internal/pool/lifecycle.go backend/internal/pool/pool.go
git commit -m "feat(pool): precious open-set, live sessions never dropped"
```

### Task I4: PIN_MODEL strict 1 akun = 1 model (R5)

**Files:**
- Create: `backend/internal/config/pin_model.go` (`parsePinModel`: `"0:z-ai/glm-5.2;1:modelB"` → `map[int]string`; tolak: multi-model per slot, index negatif, model kosong, duplikat slot)
- Delete: `backend/internal/pool/model_locks.go` (+ `model_locks_test.go`)
- Modify: `backend/internal/pool/acquire_route.go:43-54,215-226` + `acquire_order.go:47-53` (ganti `lockedOutByModel/allLockedOut` dengan `pinnedOut`)
- Modify: `backend/internal/config/config.go:92-97` (`ModelLocks map[int][]string` → `PinModel map[int]string`), `config_load.go:147,358,671`, `data.go:104-105,211-216`, `keycatalog.go:176-178`
- Modify: `frontend/src/lib/components/TokenDetailsDrawer.svelte:57-139,335-377` (editor pin-tunggal; hapus multi add/unpin list → satu select + clear)
- Test: keeper `pin_model_test.go` (parse + routing + pin-burst-5 di bawah)

**Interfaces:**
- Consumes: `registry.Registry.AgentForModel` (validasi model dikenal, pola `model_locks.go:18-42`).
- Produces:
```go
func parsePinModel(value string) (map[int]string, error)
func pinnedOut(cfg *config.Config, idx int, model string) bool
func pinFailFastError(model string, slots int) error
```
C1 memakai `pinnedOut` di `spill_order.go`; C3 memakai `PinModel` untuk UI.

- [ ] **Step 1: tulis keeper parse + pin-burst-5 (R5)**

```go
func TestParsePinModelStrict(t *testing.T) {
	got, err := parsePinModel("0:z-ai/glm-5.2;1:mimo/mimo-v2.5")
	if err != nil || len(got) != 2 || got[0] != "z-ai/glm-5.2" {
		t.Fatalf("valid = %v, %v; want 2 pins", got, err)
	}
	for _, bad := range []string{"0:a,b", "x:a", "-1:a", "0:", "0:a;0:b", "banana"} {
		if _, err := parsePinModel(bad); err == nil {
			t.Errorf("parse %q succeeded, want error", bad)
		}
	}
}

func TestPinBurst5OneAccount(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 5 * time.Second
		c.QueueDepth = 16
		c.PinModel = map[int]string{0: modelA}
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	leases := make([]*Lease, 5)
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := p.Acquire(ctx, modelA)
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			leases[i] = l
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, l := range leases {
			p.LeaseRelease(l)
		}
	}()
	running, queued := 0, 0
	for i, l := range leases {
		if l == nil {
			t.Fatalf("worker %d: no lease", i)
		}
		if l.Token != 0 {
			t.Fatalf("worker %d on account #%d, want pinned #1", i, l.Token+1)
		}
		if l.QueueWait > 0 {
			queued++
		} else {
			running++
		}
	}
	if running != 2 || queued != 3 {
		t.Fatalf("running=%d queued=%d, want 2+3 (pin burst 5)", running, queued)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("unpinned account touched (%d requests), want 0", n)
	}
}
```

Sub-kasus pin tak memakan slot model lain (slot per (akun, model)):

```go
func TestPinLeavesOtherModelSlotsFree(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 5 * time.Second
		c.QueueDepth = 16
		c.PinModel = map[int]string{0: modelA}
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var held []*Lease
	defer func() {
		for _, l := range held {
			p.LeaseRelease(l)
		}
	}()
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("modelA lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(ctx, modelB)
		if err != nil {
			t.Fatalf("modelB lease %d: %v", i, err)
		}
		if l.Token != 1 {
			t.Fatalf("modelB lease %d on account #%d, want #2 (modelA pinned to #1)", i, l.Token+1)
		}
		held = append(held, l)
	}
	for i, l := range held {
		if l.QueueWait != 0 {
			t.Fatalf("lease %d parked (%v), want zero parks (pin takes no other-model slot)", i, l.QueueWait)
		}
	}

- [ ] **Step 2: jalankan, harapkan GAGAL kompilasi** (`PinModel` belum ada; `MODEL_LOCKS` multi masih hidup)

Run: `go test ./backend/internal/pool/ -run 'TestParsePinModelStrict|TestPinBurst5OneAccount|TestPinLeavesOtherModelSlotsFree' -count=1`
Expected: FAIL kompilasi (field unknown) — itu daftar rename.

- [ ] **Step 3: implementasi pin + hapus locks**

Tulis `pin_model.go`, hapus `model_locks.go`(+test), ganti semua `lockedOutByModel/allLockedOut/lockFailFastError` dengan padanan pin, migrasi `concurrency_ladder_test.go:967-996,1102-1125` yang memakai `ModelLocks` ke `PinModel`, hapus `MODEL_LOCKS` di config/load/data/keycatalog + UI lama.

- [ ] **Step 4: jalankan, harapkan PASS — repro R5**

Run: `go test ./backend/internal/pool/ -run 'TestParsePinModelStrict|TestPinBurst5OneAccount|TestPinLeavesOtherModelSlotsFree' -count=1 -v`
Expected: PASS. **Repro R5 (pin-burst-5):** akun #1 pin modelA, burst 5 modelA → 2 jalan + 3 antre SEMUA di #1; akun #2 nol kontak; request modelB ke pool ini → `pinFailFastError` tanpa kontak upstream. **Isolasi pin:** pin modelA di #1 tidak memakan slot modelB di #1 — burst campuran 2×modelA + 2×modelB non-pinned tetap dapat slot masing-masing (modelA di #1, modelB di #2) tanpa antre.

- [ ] **Step 5: commit**

```bash
git add backend/internal/config/pin_model.go backend/internal/config/config.go backend/internal/config/config_load.go backend/internal/config/data.go backend/internal/config/keycatalog.go backend/internal/pool/acquire_route.go backend/internal/pool/acquire_order.go backend/internal/pool/pin_model_test.go frontend/src/lib/components/TokenDetailsDrawer.svelte
git rm -q backend/internal/pool/model_locks.go backend/internal/pool/model_locks_test.go
git commit -m "feat(pool): strict PIN_MODEL 1 account 1 model, delete MODEL_LOCKS"
```

### Task I5: 429 natural = request kembali antre (R6)

**Files:**
- Modify: `backend/internal/pool/spill_queue.go` (tambah `notBefore` per waiter: admission/run-start 429 kuota dengan RetryAfter → waiter kembali ke ekor lane YANG SAMA dengan `notBefore=now+retryAfter`; tanpa `CooldownRateLimit` write, tanpa failover, tanpa spill charge)
- Modify: `backend/internal/pool/acquire_route.go:418-433,523-537` (hapus `CooldownTokenRateLimit` write + `appendRateLimitEntry` failover untuk 429 kuota; teruskan `*upstream.RateLimitError` ke lane)
- Modify: `backend/internal/server/engine_attempt.go:356-378` (cabang 429: tanpa `CooldownRateLimit`, tanpa failover; surface — chat-path tidak antre ulang karena single-attempt E7; antre-ulang hanya di admission lane)
- Test: keeper `spill_requeue_test.go`: mock0 `RateLimit=true` + `RateLimitRetryAfterMs=400`; acquire → akhirnya lease `Token==0` setelah window; `mock1.RequestsSnapshot()==0` (tidak parkir akun, tidak failover); cooldown token kosong

**Interfaces:**
- Consumes: `acquireSpill` (I2); `*upstream.RateLimitError.RetryAfter` passthrough (E4); `MockUpstream{RateLimit, RateLimitRetryAfterMs, SetRateLimit}` (`mockupstream.go:123,131,720`).
- Produces: semantik "429 kuota = requeue + notBefore"; C1 mengasumsikan lane menunggu `notBefore` sebelum head mencoba lagi (tanpa kontak upstream sebelum waktunya).

- [ ] **Step 1: tulis keeper requeue (R6)**

```go
func TestNatural429RequeuesNoParkNoFailover(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	mock0.RateLimit = true
	mock0.RateLimitRetryAfterMs = 400
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 10 * time.Second
		c.QueueDepth = 16
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		time.Sleep(1200 * time.Millisecond)
		mock0.SetRateLimit(false)
	}()
	l, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v (want requeue-until-window, not surface)", err)
	}
	defer p.LeaseRelease(l)
	if l.Token != 0 {
		t.Fatalf("leased account #%d, want #1 (no failover on 429)", l.Token+1)
	}
	if n := mock1.RequestsSnapshot(); n != 0 {
		t.Fatalf("failed over to account #2 (%d requests), want 0", n)
	}
	if until := (*p.roster.Load())[0].runs.CooldownUntil(); time.Now().Before(until) {
		t.Fatalf("429 parked account until %s, want no park", until.Format(time.RFC3339))
	}
}
```

- [ ] **Step 2: jalankan, harapkan GAGAL** (429 hari ini = cooldown write + failover ke #2)

Run: `go test ./backend/internal/pool/ -run TestNatural429RequeuesNoParkNoFailover -count=1`
Expected: FAIL di `failed over` atau surface langsung.

- [ ] **Step 3: implementasi requeue**

Lane waiter membawa `notBefore`; lane head dengan `notBefore` di masa depan tidak mencoba admission (tidak ada kontak upstream) sampai tiba; habis `QUEUE_WAIT` total → spill seperti I2 (429 kuota yang tak kunjung reda tetap bisa spill — bedakan dari korelatif E1 yang surface instan). Hapus `CooldownTokenRateLimit` write di kedua jalur acquire + chat.

- [ ] **Step 4: jalankan, harapkan PASS — repro R6**

Run: `go test ./backend/internal/pool/ -run TestNatural429RequeuesNoParkNoFailover -count=1 -v`
Expected: PASS. **Repro R6 (429-kembali-antre):** window 400ms → ≤3 admission ulang di #1 lalu lease #1; #2 nol kontak; nol cooldown; pengecualian terminal: `SetBan(true)` → quarantine + request maju ke lane #2 (subtest kedua, `errors.Is(err, upstream.ErrBanned)` bila semua akun banned).

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/spill_queue.go backend/internal/pool/spill_requeue_test.go backend/internal/pool/acquire_route.go backend/internal/server/engine_attempt.go
git commit -m "feat(pool): natural 429 requeues on same lane, no park no failover"
```

---

### Task C1: Order ketat #1→#N (R1)

**Files:**
- Create: `backend/internal/pool/spill_order.go` (`spillOrder(model) []int`: index naik `[0..N)` disaring locked/quarantine/`pinnedOut`/terminal; precious holder TETAP di posisi indexnya — tidak ada re-rank, tidak ada head-boost)
- Delete: `backend/internal/pool/acquire_order.go` (+ `acquire_order_test.go`), sisa `backend/internal/pool/route_smart.go` bila hanya berisi slot (sudah pindah di I1) beserta `routeQueueHint` (`:90`) dan mapping 429-lokal
- Modify: `backend/internal/pool/spill_queue.go` (`spillLane` memakai `spillOrder`), `concurrency_ladder_test.go` (hapus ekspektasi rotasi/overflow-assist; ladder menjadi verifikasi order ketat), `queue_wait_test.go`, `queue_wait_matrix_test.go`, `route_bridge_slot_test.go` (bridge: slot saja, tanpa order — pertahankan)
- Modify: config + keycatalog + UI (hapus `TOKEN_ROTATION`, `ROUTING_SMART`)
- Test: keeper `spill_order_test.go`: 3 akun idle → 10 acquire sekuensial SEMUA `Token==0` sampai penuh, lalu #2, lalu #3 (drain murni index; bukan round-robin)

**Interfaces:**
- Consumes: `pinnedOut` (I4); `preciousHas` (I3, hanya untuk label/telemetri — TIDAK untuk re-rank); `spillLane/acquireSpill` (I2).
- Produces:
```go
func (p *Pool) spillOrder(model string) []int
```
Urutan ini satu-satunya ranking di pool (R1); tidak ada caller lain yang boleh mengurutkan ulang.

- [ ] **Step 1: tulis keeper drain-index (R1)**

```go
func TestStrictOrderDrainsAccountOneFirst(t *testing.T) {
	mocks := make([]*testutil.MockUpstream, 3)
	for i := range mocks {
		mocks[i] = testutil.NewMock()
		t.Cleanup(mocks[i].Close)
	}
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.QueueWait = 300 * time.Millisecond
		c.QueueDepth = 16
	}, mocks[0], mocks[1], mocks[2])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var held []*Lease
	defer func() {
		for _, l := range held {
			p.LeaseRelease(l)
		}
	}()
	for i := 0; i < 6; i++ {
		l, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("lease %d: %v", i, err)
		}
		held = append(held, l)
	}
	for i, l := range held {
		want := i / 2
		if l.Token != want {
			t.Fatalf("lease %d on account #%d, want #%d (strict #1..#N)", i, l.Token+1, want+1)
		}
		if l.QueueWait != 0 {
			t.Fatalf("lease %d parked (%v), want zero parks (capacity free)", i, l.QueueWait)
		}
	}
}
```

- [ ] **Step 2: jalankan, harapkan GAGAL** (sisa rr/scorer/lastUsed menyebar)

Run: `go test ./backend/internal/pool/ -run TestStrictOrderDrainsAccountOneFirst -count=1`
Expected: FAIL di lease awal (bukan #1 berurutan).

- [ ] **Step 3: kunci order**

Tulis `spill_order.go`, hapus `acquire_order.go`(+test) dan sisa `route_smart.go` (pastikan `slot_*` sudah pindah I1; hapus `routeQueueHint` + mapping 429-lokal — timeout antrean hanya sinyal internal spill), hapus knobs `TOKEN_ROTATION`/`ROUTING_SMART` di semua lapisan, migrasi ladder/queue/bridge test.

- [ ] **Step 4: jalankan, harapkan PASS — repro R1**

Run: `go test ./backend/internal/pool/ -run TestStrictOrderDrainsAccountOneFirst -count=1 -v`
Expected: PASS. **Repro R1:** 6 acquire sekuensial 3 akun → token `[0,0,1,1,2,2]`, nol parkir; reorder dashboard (swap akun) → urutan mengikuti index baru (subtest: `RemoveLastToken`/reorder lalu ulangi, pola `pool_remove_test.go`/`pool_swap_test.go`).

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/spill_order.go backend/internal/pool/spill_order_test.go backend/internal/pool/spill_queue.go backend/internal/pool/concurrency_ladder_test.go backend/internal/pool/queue_wait_test.go backend/internal/config/ frontend/src/lib/utils/poolStrategy.js
git rm -q backend/internal/pool/acquire_order.go backend/internal/pool/acquire_order_test.go backend/internal/pool/route_smart.go
git commit -m "feat(pool): strict account order 1..N, delete rotation and smart routing"
```

### Task C2: Knobs final + presets strategi

**Files:**
- Modify: `backend/internal/config/keycatalog.go` (entri final 5 knob + deskripsi; hapus: `MODEL_LOCKS`, `TOKEN_ROTATION`, `ROUTING_SMART`, `RATE_LIMIT_FAILOVER`, `TOKEN_MAX_CONCURRENT`, `COOLDOWN_*`, `SESSION_PARK_*`, `SESSION_POLL_MAX_MS`, `SMART_PROBE_*`, `MATURITY_*`, `QUOTA_PROBE_*`)
- Modify: `backend/internal/config/config_validate.go` (validasi silang: `PIN_MODEL` vs slot ada; `MAX_SPILL_ACCOUNTS >= 0`; `SLOTS_PER_ACCOUNT >= 1` per (akun, model) atau 0=unlimited; `QUEUE_DEPTH >= 0`)
- Modify: `frontend/src/lib/utils/poolStrategy.js` (`STRATEGY_OWNED_KEYS` → 5 knob final; Drain = `QUEUE_WAIT 300s / QUEUE_DEPTH 1024`; Balance = threshold 5–300s default 60s sebagai `QUEUE_WAIT` + depth 16)
- Modify: `frontend/src/lib/components/StrategyPresetCard.svelte` + `TrafficSettings.svelte` + `AdvancedSettings.svelte` (satu editor per knob; hapus baris mati)
- Test: `backend/internal/config/config_knobs_test.go` + `routing_knobs_test.go` (tambah 5 knob; hapus mati) + `keycatalog_test.go` (set kunci)

**Interfaces:**
- Consumes: `parsePinModel` (I4); `slotParams` (I1); `spillLane` (I2).
- Produces: kontrak settings final untuk C3; tidak ada simbol baru.

- [ ] **Step 1: tulis test katalog**

```go
func TestFinalKnobCatalog(t *testing.T) {
	for _, k := range []string{"SLOTS_PER_ACCOUNT", "QUEUE_WAIT", "QUEUE_DEPTH", "PIN_MODEL", "MAX_SPILL_ACCOUNTS"} {
		if !keycatalog.Has(k) {
			t.Errorf("knob %s missing from catalog", k)
		}
	}
	for _, k := range []string{"MODEL_LOCKS", "TOKEN_ROTATION", "ROUTING_SMART", "RATE_LIMIT_FAILOVER", "TOKEN_MAX_CONCURRENT", "COOLDOWN_DEFAULT_MS", "SESSION_PARK_ENABLED", "MATURITY_ENABLED", "QUOTA_AUTO_PROBE"} {
		if keycatalog.Has(k) {
			t.Errorf("dead knob %s still catalogued", k)
		}
	}
}
```

(Sesuaikan dengan accessor katalog aktual di `keycatalog.go` — JANGAN buat helper baru; bila katalog berupa slice, iterasi slice.)

- [ ] **Step 2: jalankan, harapkan GAGAL** (kunci mati masih ada, kunci baru belum)

Run: `go test ./backend/internal/config/ -run TestFinalKnobCatalog -count=1`
Expected: FAIL.

- [ ] **Step 3: finalisasi katalog + validasi + preset**

Hapus/tambah entri, tulis validasi silang, update `poolStrategy.js` owned keys + preset Drain/Balance + copy `StrategyPresetCard`/`TrafficSettings`/`AdvancedSettings` (satu editor per knob, tidak ada duplikat writer).

- [ ] **Step 4: jalankan, harapkan PASS**

Run: `go test ./backend/internal/config/ -run 'TestFinalKnobCatalog|TestConfig' -count=1`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add backend/internal/config/keycatalog.go backend/internal/config/config_validate.go backend/internal/config/config_knobs_test.go frontend/src/lib/utils/poolStrategy.js frontend/src/lib/components/StrategyPresetCard.svelte frontend/src/lib/pages/TrafficSettings.svelte frontend/src/lib/pages/settings/AdvancedSettings.svelte
git commit -m "feat(config): final Ordered Queue-Spill knobs and strategy presets"
```

### Task C3: UI PIN per akun

**Files:**
- Modify: `frontend/src/lib/components/TokenDetailsDrawer.svelte:57-139,335-377` (satu select model + tombol Clear → `PIN_MODEL` slot syntax via settings overlay POST; hapus multi-pin list)
- Modify: `frontend/src/lib/pages/Tokens.svelte:62-86` (chip ringkas: pin model per akun; hapus referensi `TOKEN_ROTATION`/`RATE_LIMIT_FAILOVER`/`ROUTING_SMART`)
- Test: e2e `frontend/e2e/` pin test (pola pin+unpin yang ada per CurrentControl: ganti ke single-pin; assert `PIN_MODEL` value `"0:<model>"` + clear kembali `""`)

**Interfaces:**
- Consumes: settings overlay POST (`adminApi.settingsSave`, pola `TokenDetailsDrawer.svelte:105-111`); `allowed_models` snapshot diganti `pinned_model` tunggal (tambah field di `TokenSnapshot` `pool.go:229-232` + `snapshot.go:200-202` pada task ini).
- Produces: tidak ada simbol baru; e2e menjadi bukti R5 ujung-ke-ujung.

- [ ] **Step 1: tulis e2e single-pin**

```ts
test("pin account to one model and clear", async ({ page }) => {
  await openTokenDrawer(page, 0);
  await selectPinModel(page, "z-ai/glm-5.2");
  await expectSettingsValue(page, "PIN_MODEL", "0:z-ai/glm-5.2");
  await clearPinModel(page);
  await expectSettingsValue(page, "PIN_MODEL", "");
});
```

(Nama helper mengikuti file e2e yang ada — JANGAN buat framework baru; bila helper bernama lain, pakai nama aktualnya dengan langkah yang sama.)

- [ ] **Step 2: jalankan, harapkan GAGAL** (UI multi-lock lama tidak menulis `PIN_MODEL`)

Run: `npm --prefix frontend run test:e2e -- pin` (nama skrip mengikuti `package.json` aktual)
Expected: FAIL (key tidak ditemukan / value salah).

- [ ] **Step 3: implementasi editor + snapshot field**

Satu select + Clear di drawer; `TokenSnapshot.pinned_model`; Tokens chips; hapus copy knob mati.

- [ ] **Step 4: jalankan, harapkan PASS**

Run: skrip e2e yang sama
Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add frontend/src/lib/components/TokenDetailsDrawer.svelte frontend/src/lib/pages/Tokens.svelte backend/internal/pool/pool.go backend/internal/pool/snapshot.go frontend/e2e/
git commit -m "feat(dashboard): single-model PIN editor per account"
```

### Task C4: Promosi keeper + bersih + dokumen perilaku

**Files:**
- Modify: `backend/internal/pool/concurrency_ladder_test.go`, `queue_wait_test.go`, `queue_wait_matrix_test.go` (hapus ekspektasi overflow-assist/rotasi/cooldown; jadikan ladder verifikasi R1–R3)
- Keep (promosi dari throwaway): `slot_ledger_test.go` (R2), `spill_queue_test.go` (R3), `precious_test.go` (R4), `pin_model_test.go` (R5), `spill_requeue_test.go` (R6), `spill_order_test.go` (R1), subtest nol-probe E5 + ban-keeper E4
- Delete: sisa throwaway E1–E3/E7–E8 yang tidak dipromosi
- Modify: `frontend/src/lib/components/LiveConsole.svelte:179-183` (copy QUEUED → spill lane), keycatalog descriptions (dokumen perilaku adalah deskripsi knob + test)

**Interfaces:**
- Consumes: semua task E/I/C.
- Produces: suite hijau pada revisi ini; tidak ada simbol baru.

- [ ] **Step 1: daftarkan keeper final dan hapus throwaway sisa**

```bash
git status --short backend/internal/pool/*spill* backend/internal/pool/*slot* backend/internal/pool/*precious* backend/internal/pool/*pin*
```

Pastikan hanya file keeper yang tertinggal; hapus throwaway yang tidak dipromosi (`spill_excise_nochatretry_test.go` setelah E7 terbukti, dll.).

- [ ] **Step 2: jalankan keeper R1–R6 berurutan**

Run: `go test ./backend/internal/pool/ -run 'TestStrictOrderDrainsAccountOneFirst|TestSlotLedgerTwoSlotsThirdParks|TestSlotLedgerPerModelIsolation|TestSpillBurst5TwoAccounts|TestPreciousTwoAccountsSameModel|TestPinBurst5OneAccount|TestPinLeavesOtherModelSlotsFree|TestNatural429RequeuesNoParkNoFailover' -count=1 -v`
Expected: 8/8 PASS — ini bukti acceptance R1–R6.

- [ ] **Step 3: migrasi ladder + queue test lama**

Hapus cabang ekspektasi `rotation`/`overflow`/`cooldown` di `concurrency_ladder_test.go` (termasuk `setLadderRotation`, `ladTripWindow`, `ladQuarantineViaBan` bila mengasumsikan mesin lama — quarantine ban DIPERTAHANKAN jadi `ladQuarantineViaBan` tetap dengan ekspektasi tanpa-cooldown), `queue_wait_test.go`, `queue_wait_matrix_test.go`.

- [ ] **Step 4: jalankan ulang keeper + update copy UI**

Run keeper yang sama + `npm --prefix frontend run format:check`
Expected: PASS semua; copy LiveConsole menyebut spill lane bukan 429-lokal.

- [ ] **Step 5: commit**

```bash
git add backend/internal/pool/ frontend/src/lib/components/LiveConsole.svelte
git commit -m "feat(pool): promote Ordered Queue-Spill keepers, retire legacy expectations"
```

---

## Traceability R1–R6 → Task

| R | Perilaku | Task pelaksana | Repro |
|---|---|---|---|
| R1 | order ketat #1→#N satu-satunya ranking | C1 (`spill_order.go`); fondasi E6 | `TestStrictOrderDrainsAccountOneFirst` |
| R2 | 2 slot concurrent per (akun, model) — 1 akun boleh 2×A + 2×B bareng | I1 (`slot_ledger.go` keyed (token, model), `SLOTS_PER_ACCOUNT`) | `TestSlotLedgerTwoSlotsThirdParks` (3-req-model-sama) + `TestSlotLedgerPerModelIsolation` (2×A + 2×B = 4 jalan) |
| R3 | spill HANYA saat queue-wait habis | I2 (`spill_queue.go`, `MAX_SPILL_ACCOUNTS`) | `TestSpillBurst5TwoAccounts` (burst-5-2-akun) |
| R4 | precious tidak pernah di-drop; N akun × model = 2N slot | I3 (`precious.go`); fondasi E8 | `TestPreciousTwoAccountsSameModel` |
| R5 | PIN_MODEL explicit 1 akun = 1 model | I4 (`pin_model.go`); UI C3 | `TestPinBurst5OneAccount` (pin-burst-5) + `TestPinLeavesOtherModelSlotsFree` (pin tak makan slot model lain) + e2e C3 |
| R6 | 429 natural = kembali antre, akun tidak diparkir (kecuali banned/suspend) | I5 (requeue+notBefore); fondasi E1/E2/E4 (korelatif surface, tanpa cooldown); E7 (tanpa retry kedua) | `TestNatural429RequeuesNoParkNoFailover` (429-kembali-antre) + `TestExciseBanStillQuarantined` |

## Self-Review

1. **Spec coverage:** R1→C1, R2→I1, R3→I2, R4→I3(+E8), R5→I4(+C3), R6→I5(+E1/E2/E4/E7). Knobs `SLOTS_PER_ACCOUNT` (cap per (akun, model), MASQ)/`QUEUE_WAIT`/`PIN_MODEL`/`MAX_SPILL_ACCOUNTS`→I1/I2/I4/C2; `QUEUE_DEPTH` dipertahankan I1. Excision a→E1, b→E2, c→E3, d→KEEP di E4, e→E4, f+g→E5, h→E6, i→E7, j→E8, k→I2 (timeout = sinyal spill internal, 429-lokal dihapus). Tidak ada gap.
2. **Placeholder scan:** tanpa TBD/TODO/"mirip Task N" — setiap step membawa kode aktual, angka ekspektasi eksplisit, dan perintah run. Rujukan "nama aktual" hanya untuk helper yang memang harus dibaca dari file tetangga saat eksekusi (MaintainOnce, helper e2e, accessor katalog), dengan fallback eksplisit dilarang membuat yang baru.
3. **Type consistency:** `slotAcquire/slotPermit/slotQueueExhaustedError`, `spillLane/acquireSpill/spillOrder`, `parsePinModel/pinnedOut/pinFailFastError`, `preciousAdd/preciousHas/preciousRemove` didefinisikan sekali di task produsen dan dipakai dengan nama yang sama di konsumen. `Lease{Token, QueueWait, SessionInstanceID}`, `p.LeaseRelease`, `newTestPoolCfg`, `testutil.MockUpstream` field/method semuanya terverifikasi di revisi worktree.
