package upstream

import (
	"errors"
	"net/http"
	"testing"
)

// Issue #630: a 404 "No endpoints ..." routing refusal must classify to
// the distinct NoEndpointsError — never the generic 502
// upstream_unavailable, never a cooldown-bearing type. Mock-level only:
// the live trigger split (routing fence vs capacity) needs a live dump.

func TestClassifyNoEndpoints(t *testing.T) {
	body := `{"error":{"message":"No endpoints found for deepseek/deepseek-v4-flash.","code":404,"type":null,"param":null}}`
	err := classifyError(http.StatusNotFound, body, http.Header{})
	var nee *NoEndpointsError
	if !errors.As(err, &nee) {
		t.Fatalf("classifyError = %T %v, want *NoEndpointsError", err, err)
	}
	if !errors.Is(err, ErrNoEndpoints) {
		t.Errorf("errors.Is(ErrNoEndpoints) = false, got %v", err)
	}
	if nee.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", nee.Status)
	}
	if nee.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("Model = %q, want deepseek/deepseek-v4-flash", nee.Model)
	}
	if errClassName(err) != "NoEndpointsError" {
		t.Errorf("errClassName = %q, want NoEndpointsError", errClassName(err))
	}
	// Model-scoped routing refusal: must not read as session, run,
	// rate-limit, or any other stateful signal.
	for _, s := range []error{ErrSessionInvalid, ErrRunInvalid, ErrRateLimited, ErrBanned, ErrIpCapped} {
		if errors.Is(err, s) {
			t.Errorf("no-endpoints matched stateful sentinel %v", s)
		}
	}
}

func TestClassifyNoEndpointsVariants(t *testing.T) {
	// Marker on a non-404 stays generic: the 404 gate stays tight, the
	// same marker on any other status falls to default.
	if err := classifyError(http.StatusBadGateway, `{"error":"no endpoints"}`, http.Header{}); errors.Is(err, ErrNoEndpoints) {
		t.Errorf("502 no-endpoints body matched the 404 arm: %v", err)
	}
	// A bare 404 without the marker stays the generic UpstreamError.
	err := classifyError(http.StatusNotFound, `{"error":"not_found"}`, http.Header{})
	var nee *NoEndpointsError
	if errors.As(err, &nee) {
		t.Errorf("marker-less 404 matched the no-endpoints arm: %v", err)
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Errorf("marker-less 404 = %T, want *UpstreamError", err)
	}
	// Missing model id leaves Model empty rather than failing the arm.
	err = classifyError(http.StatusNotFound, `{"error":"No endpoints available"}`, http.Header{})
	if !errors.As(err, &nee) || nee.Model != "" {
		t.Errorf("model-less body = %v, want *NoEndpointsError with empty Model", err)
	}
}
