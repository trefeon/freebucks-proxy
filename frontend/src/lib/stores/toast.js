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
}
