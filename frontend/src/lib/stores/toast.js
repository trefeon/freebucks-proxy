import { writable, get } from "svelte/store";

// Global toast notifications: the single non-blocking channel for
// transient success/info/warning/error feedback. Blocking, persistent, or
// contextual states (session-expired, default-password, DB-overlay
// degraded, upstream drift, maturity kill-switch) stay inline as <Alert>.
const MAX_VISIBLE = 4;
// Every toast — error and warning included — leaves on its own after 10s.
// Tone never decides lifetime: only an explicit `sticky: true` opts out.
const AUTO_DISMISS_MS = 10000;

let nextId = 1;
const timers = new Map();

/** Currently rendered toasts (at most MAX_VISIBLE). */
export const toasts = writable([]);
/** Overflow FIFO: promoted in order as visible slots free up. */
export const toastQueue = writable([]);

function schedule(t) {
  if (t.sticky) return;
  clearTimeout(timers.get(t.id));
  timers.set(
    t.id,
    setTimeout(() => dismiss(t.id), AUTO_DISMISS_MS),
  );
}

function pump() {
  if (get(toasts).length >= MAX_VISIBLE) return;
  const q = get(toastQueue);
  if (q.length === 0) return;
  const [first, ...rest] = q;
  toastQueue.set(rest);
  toasts.set([...get(toasts), first]);
  schedule(first);
}

/**
 * Push a toast. It fades after 10s regardless of tone unless
 * `sticky: true` is passed; that flag is the only escape hatch, and it is
 * for blocking states that must not vanish on their own.
 *
 * @param {{ tone?: 'info'|'success'|'warning'|'error', title?: string, body?: string, sticky?: boolean }} toast
 * @returns {number} toast id for dismiss()
 */
export function push({ tone = "info", title = "", body = "", sticky } = {}) {
  const toast = { id: nextId++, tone, title, body, sticky };
  if (get(toasts).length < MAX_VISIBLE) {
    toasts.set([...get(toasts), toast]);
    schedule(toast);
  } else {
    toastQueue.set([...get(toastQueue), toast]);
  }
  return toast.id;
}

export function dismiss(id) {
  clearTimeout(timers.get(id));
  timers.delete(id);
  markNotifyGone(id);
  let removed = false;
  toasts.update((list) => {
    const next = list.filter((t) => t.id !== id);
    if (next.length !== list.length) removed = true;
    return next;
  });
  if (removed) pump();
  else toastQueue.update((q) => q.filter((t) => t.id !== id));
}

export function clear() {
  for (const timer of timers.values()) clearTimeout(timer);
  timers.clear();
  toasts.set([]);
  toastQueue.set([]);
  notifyState.clear();
}

// Mutation-toast discipline (Phase 4 R5): the single shared convention for
// error surfaces. Every panel keys its error toast; the same key+message
// renders exactly one toast no matter how often the poll re-fails, and a
// manual dismiss is respected until the message actually changes (the entry
// survives dismiss, so re-failing with the identical text stays silent).
// Panels whose inline state already shows the error call clearNotify on
// success; auto-poll ticks skip the toast entirely (inline only).
/** key -> { id, title, body, live } */
const notifyState = new Map();

function markNotifyGone(id) {
  for (const s of notifyState.values()) {
    if (s.id === id) s.live = false;
  }
}

/**
 * Push (or keep) the keyed error toast. Same key+text while live or
 * user-dismissed is a no-op — only a changed message replaces it.
 * @param {string} key - stable per-surface key (e.g. 'logs', 'traces')
 * @param {{ tone?: 'info'|'success'|'warning'|'error', title?: string, body?: string, sticky?: boolean }} toast
 * @returns {number} toast id (0 when nothing is shown)
 */
export function notifyOnce(
  key,
  { tone = "error", title = "", body = "" } = {},
) {
  if (!title && !body) {
    clearNotify(key);
    return 0;
  }
  const prev = notifyState.get(key);
  if (prev && prev.title === title && (prev.body ?? "") === (body ?? "")) {
    return prev.id;
  }
  if (prev && prev.live) dismiss(prev.id);
  const id = push({ tone, title, body });
  notifyState.set(key, { id, title, body: body ?? "", live: true });
  return id;
}

/**
 * Dismiss the keyed toast and forget its text, so the next failure toasts
 * again. Call on success (and unmount for poll-owned keys).
 * @param {string} key
 */
export function clearNotify(key) {
  const prev = notifyState.get(key);
  notifyState.delete(key);
  if (prev && prev.live) dismiss(prev.id);
}
