import { writable } from "svelte/store";
import { isSessionDead } from "./session.js";

// Shared query store behind every rewired page. One owner per endpoint: a
// refcounted poll loop (visibility-aware, dead-session gated, race-safe)
// plus an optional push subscription (SSE). Pages render from `data` and
// never wire their own timers.
//
// Full/live split: endpoints that serve a cheap live view pass `fetchLive`
// with a `merge(cached, live)` that overlays it on the last full snapshot
// (the Overview/Tokens static-key pattern, generalized). A full refresh runs
// on first poll, every FULL_EVERY_POLLS polls, every FULL_EVERY_MS, and on
// every `refresh()` call (page mutations call it instead of waiting a tick).
//
// Race-safe lifecycle: every in-flight request owns an AbortController and a
// monotonic sequence number. Completions apply only when their seq is still
// current, so a slow earlier response (or a push that landed mid-flight)
// can never regress newer data. A refresh() arriving mid-flight queues one
// full pass instead of dropping (the old busy-drop) or overlapping.
// Consecutive failures back off the cadence instead of hammering.

const FULL_EVERY_POLLS = 30;
const FULL_EVERY_MS = 5 * 60 * 1000;
// Error backoff: each consecutive failure doubles the next delay from the
// base interval up to this cap; a success resets the chain.
const BACKOFF_CAP_MS = 60 * 1000;

/**
 * @param {Object} opts
 * @param {number} opts.intervalMs - hot poll interval
 * @param {(signal: AbortSignal) => Promise<any>} opts.fetchFull - full-shape fetch
 * @param {((signal: AbortSignal) => Promise<any>)} [opts.fetchLive] - cheap live-view fetch
 * @param {(cached: any, live: any) => any} [opts.merge] - overlay live onto cached
 * @param {(next: (v: any) => void) => (() => void) | void} [opts.subscribe] - push subscription, returns cleanup
 */
export function createQueryStore({
  intervalMs,
  fetchFull,
  fetchLive,
  merge,
  subscribe,
}) {
  const data = writable(null);
  const error = writable("");

  let consumers = 0;
  let timer = null;
  let polls = 0;
  let lastFullAt = 0;
  let unsubPush = null;
  let seq = 0;
  let activeAbort = null;
  let queuedRefresh = false;
  let consecutiveErrors = 0;

  function remember(full) {
    lastFullAt = Date.now();
    data.set(full);
  }

  function scheduleNext() {
    clearTimeout(timer);
    timer = null;
    if (consumers === 0) return;
    const delay =
      consecutiveErrors === 0
        ? intervalMs
        : Math.min(
            intervalMs * 2 ** Math.min(consecutiveErrors, 6),
            BACKOFF_CAP_MS,
          );
    timer = setTimeout(() => {
      timer = null;
      void poll(false);
    }, delay);
  }

  async function poll(full) {
    if (isSessionDead()) return;
    if (activeAbort) {
      // One request at a time: a timer tick never piles on, and a manual
      // refresh queues exactly one full pass for when the flight lands.
      if (full) queuedRefresh = true;
      return;
    }
    const stale =
      full ||
      lastFullAt === 0 ||
      polls % FULL_EVERY_POLLS === 0 ||
      Date.now() - lastFullAt > FULL_EVERY_MS;
    const mySeq = (seq += 1);
    const ctrl = new AbortController();
    activeAbort = ctrl;
    try {
      polls += 1;
      if (!fetchLive || !merge || stale) {
        const v = await fetchFull(ctrl.signal);
        if (mySeq === seq) remember(v);
      } else {
        const live = await fetchLive(ctrl.signal);
        if (mySeq === seq) data.update((cached) => merge(cached, live));
      }
      // A superseded pass stays silent even on success: the newer data
      // (push or queued refresh) already owns the view.
      if (mySeq === seq) error.set("");
      consecutiveErrors = 0;
    } catch (e) {
      // AbortError = this pass was superseded (refresh-while-flying aborts
      // it below) — never surface it as a page error.
      if (e?.name === "AbortError") return;
      if (!isSessionDead() && mySeq === seq) error.set(e?.message ?? String(e));
      consecutiveErrors += 1;
    } finally {
      if (activeAbort === ctrl) activeAbort = null;
      if (consumers > 0) scheduleNext();
      if (queuedRefresh) {
        queuedRefresh = false;
        void poll(true);
      }
    }
  }

  function stopTimer() {
    clearTimeout(timer);
    timer = null;
  }

  function handleVisibility() {
    if (document.hidden) {
      stopTimer();
    } else if (consumers > 0) {
      if (activeAbort) {
        // Resume landed mid-flight: queue one full pass for when it
        // lands instead of piling on (or dropping the resume).
        queuedRefresh = true;
      } else {
        // The FULL_EVERY_MS gate inside poll forces a full fetch when the
        // cache aged past it (notably after a >5min hide), so resume never
        // merges a live view onto stale static.
        void poll(false);
      }
      scheduleNext();
    }
  }

  function start() {
    polls = 0;
    lastFullAt = 0;
    consecutiveErrors = 0;
    void poll(false);
    scheduleNext();
    document.addEventListener("visibilitychange", handleVisibility);
    if (subscribe) {
      try {
        // A push is newer than anything in flight: bump the sequence so a
        // stale in-flight completion cannot regress it when it lands.
        unsubPush =
          subscribe((v) => {
            seq += 1;
            remember(v);
          }) ?? null;
      } catch {
        unsubPush = null;
      }
    }
  }
  function stop() {
    stopTimer();
    activeAbort?.abort();
    activeAbort = null;
    queuedRefresh = false;
    document.removeEventListener("visibilitychange", handleVisibility);
    unsubPush?.();
    unsubPush = null;
  }

  /**
   * Reference-counted activation: each mounted page calls this once and
   * calls the returned release on unmount. The loop runs while at least one
   * consumer holds it.
   */
  function ensure() {
    consumers += 1;
    if (consumers === 1) start();
    return () => {
      consumers = Math.max(0, consumers - 1);
      if (consumers === 0) stop();
    };
  }

  /** Force a full refresh now (page mutations call this). */
  function refresh() {
    if (activeAbort) {
      // Mid-flight: abort the stale flight so its completion cannot
      // regress the fresh pass, then run the full refresh now.
      activeAbort.abort();
      activeAbort = null;
      queuedRefresh = false;
      return poll(true);
    }
    return poll(true);
  }

  return { data, error, ensure, refresh };
}
