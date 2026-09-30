package dashboard

import (
	"testing"
)

func TestNoticesData(t *testing.T) {
	d := &Dashboard{}
	resp := d.noticesData()

	// The upstream tier-change announcement retired with vendor a2fd480
	// (upstream deleted FREEBUFF_TIER_CHANGE_NOTICE): the card must stay
	// gone. Peak windows and per-token broadcasts still ride this payload.
	for _, n := range resp.Notices {
		if n.ID == "upstream-tier-change" {
			t.Errorf("retired upstream-tier-change card still served in noticesData()")
		}
	}
	if resp.Count != len(resp.Notices) {
		t.Errorf("noticesData() Count = %d, want len(Notices) = %d", resp.Count, len(resp.Notices))
	}
}
