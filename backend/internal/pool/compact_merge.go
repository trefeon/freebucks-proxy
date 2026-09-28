package pool

import (
	"freebucks-proxy/backend/internal/session"
)

// mergeCompactSessionSnapshot delegates to session.MergeCompactSnapshot,
// the single port of the vendor compact merge (upstream/freebuff
// cli/src/utils/freebuff-session-api.ts, mergeCompactActiveSession).
// Carried fields: rateLimitsByModel → QuotaByModel, subscription →
// SubscriptionTierID, freebucks → Freebucks; the vendor's singular
// rateLimit has no proxy counterpart and stays unmapped. keepCompact=false
// mirrors the vendor's null return: fetch one full response before
// compacting again.
func mergeCompactSessionSnapshot(current, next session.SessionSnapshot) (merged session.SessionSnapshot, keepCompact bool) {
	return session.MergeCompactSnapshot(current, next)
}
