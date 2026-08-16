package omnigent

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestBoundaryOutcomeMatrixUsesOnlyTypedMachineFacts(t *testing.T) {
	tests := []struct {
		name string
		got  OutcomeKind
		want OutcomeKind
	}{
		{"exists", ClassifyError(nil), OutcomeExists},
		{"not found", ClassifyError(&APIError{StatusCode: http.StatusNotFound}), OutcomeNotFound},
		{"initializing", ClassifySessionStatus("initializing"), OutcomeInitializing},
		{"unavailable", ClassifyError(&APIError{StatusCode: http.StatusServiceUnavailable}), OutcomeUnavailable},
		{"incompatible version", ClassifyError(&APIError{Code: "schema_mismatch"}), OutcomeIncompatibleVersion},
		{"invalid profile", ClassifyError(&APIError{Code: "invalid_profile"}), OutcomeInvalidProfile},
		{"auth failure", ClassifyStreamEvent(errorEvent("authentication_failed", 401)), OutcomeAuthFailure},
		{"rate limit", ClassifyStreamEvent(errorEvent("rate_limit", 429)), OutcomeRateLimit},
		{"fallback exhausted", ClassifyStreamEvent(errorEvent("fallback_exhausted", 0)), OutcomeFallbackExhausted},
		{"policy pending", ClassifyStreamEvent(StreamEvent{Type: "policy.request"}), OutcomePolicyPending},
		{"timeout", ClassifyError(fmt.Errorf("wait: %w", context.DeadlineExceeded)), OutcomeTimeout},
		{"cancellation", ClassifyError(fmt.Errorf("read: %w", context.Canceled)), OutcomeCanceled},
		{"protocol error", ClassifyError(&APIError{StatusCode: http.StatusBadRequest, Code: "unknown_wire_code"}), OutcomeProtocolError},
		{"harness exit", ClassifyStreamEvent(StreamEvent{Type: "harness.exit"}), OutcomeHarnessExit},
		{"attachment loss", ClassifyStreamEvent(StreamEvent{Type: "attachment.lost"}), OutcomeAttachmentLoss},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("outcome = %q, want %q", tt.got, tt.want)
			}
		})
	}

	messageOnly := StreamEvent{Type: "error", Error: &StreamError{Code: "", Message: "authentication_failed rate_limit"}}
	if got := ClassifyStreamEvent(messageOnly); got != OutcomeProtocolError {
		t.Fatalf("free-form message changed classification: %q", got)
	}
}

func errorEvent(code string, status int) StreamEvent {
	return StreamEvent{Type: "error", Error: &StreamError{Code: code, Detail: &StreamErrorDetail{StatusCode: status}}}
}
