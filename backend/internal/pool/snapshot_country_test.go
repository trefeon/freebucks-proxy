package pool

import (
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"testing"
)

// TestCloneStringsDetaches pins the copy contract: nil/empty stays nil
// (omitempty shape unchanged), and a non-empty clone never aliases the
// source slice.
func TestCloneStringsDetaches(t *testing.T) {
	if got := cloneStrings(nil); got != nil {
		t.Errorf("cloneStrings(nil) = %v, want nil", got)
	}
	if got := cloneStrings([]string{}); got != nil {
		t.Errorf("cloneStrings(empty) = %v, want nil", got)
	}
	src := []string{"vpn", "proxy"}
	got := cloneStrings(src)
	src[0] = "mutated"
	if got[0] != "vpn" {
		t.Errorf("clone aliases source: got %v after source mutation", got)
	}
}

// TestSnapshotCarriesCountryMismatchFields pins the mismatch surfacing: a
// remembered country block overrides the session view with the blocking
// country + reason + privacy signals, detached from the live error.
func TestSnapshotCarriesCountryMismatchFields(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.CountryCode = "US"
	p := newTestPool(t, mock)

	cbe := &upstream.CountryBlockedError{
		CountryCode:        "XX",
		CountryBlockReason: "anonymized_or_unknown_country",
		IpPrivacySignals:   []string{"vpn", "proxy"},
	}
	p.CooldownTokenCountryBlocked(0, cbe)
	snap := p.Snapshot()[0]
	if snap.CountryCode != "XX" {
		t.Errorf("CountryCode = %q, want remembered block XX", snap.CountryCode)
	}
	if snap.CountryBlockReason != "anonymized_or_unknown_country" {
		t.Errorf("CountryBlockReason = %q, want the remembered reason", snap.CountryBlockReason)
	}
	if len(snap.IPPrivacySignals) != 2 || snap.IPPrivacySignals[0] != "vpn" || snap.IPPrivacySignals[1] != "proxy" {
		t.Errorf("IPPrivacySignals = %v, want [vpn proxy]", snap.IPPrivacySignals)
	}
	// Detached: mutating the live error must not rewrite the snapshot.
	cbe.IpPrivacySignals[0] = "mutated"
	if snap.IPPrivacySignals[0] != "vpn" {
		t.Errorf("snapshot aliases the live block: %v", snap.IPPrivacySignals)
	}
}

// TestSnapshotOmitsAbsentMismatchFields pins the clean-account shape: no
// block, no signals - the warning fields stay empty (omitempty omits them
// on the wire).
func TestSnapshotOmitsAbsentMismatchFields(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	snap := p.Snapshot()[0]
	if snap.CountryBlockReason != "" {
		t.Errorf("CountryBlockReason = %q, want empty with no block", snap.CountryBlockReason)
	}
	if snap.IPPrivacySignals != nil {
		t.Errorf("IPPrivacySignals = %v, want nil with no signals", snap.IPPrivacySignals)
	}
}
