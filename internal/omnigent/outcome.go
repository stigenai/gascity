package omnigent

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// OutcomeKind is a stable, judgment-free classifier for facts observed at the
// local Omnigent boundary. It does not imply retry, routing, or work policy.
type OutcomeKind string

// Outcome values identify facts observed at the Omnigent boundary.
const (
	OutcomeExists              OutcomeKind = "exists"
	OutcomeNotFound            OutcomeKind = "not_found"
	OutcomeInitializing        OutcomeKind = "initializing"
	OutcomeUnavailable         OutcomeKind = "unavailable"
	OutcomeIncompatibleVersion OutcomeKind = "incompatible_version"
	OutcomeInvalidProfile      OutcomeKind = "invalid_profile"
	OutcomeAuthFailure         OutcomeKind = "auth_failure"
	OutcomeRateLimit           OutcomeKind = "rate_limit"
	OutcomeFallbackExhausted   OutcomeKind = "fallback_exhausted"
	OutcomePolicyPending       OutcomeKind = "policy_pending"
	OutcomeTimeout             OutcomeKind = "timeout"
	OutcomeCanceled            OutcomeKind = "canceled"
	OutcomeProtocolError       OutcomeKind = "protocol_error"
	OutcomeHarnessExit         OutcomeKind = "harness_exit"
	OutcomeAttachmentLoss      OutcomeKind = "attachment_loss"
	OutcomeUnknown             OutcomeKind = "unknown"
)

// ClassifyError translates typed transport and context errors into a stable
// boundary fact. Unknown errors stay unknown; callers retain the original
// error for full context.
func ClassifyError(err error) OutcomeKind {
	if err == nil {
		return OutcomeExists
	}
	if errors.Is(err, context.Canceled) {
		return OutcomeCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeTimeout
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if outcome := outcomeForCode(apiErr.Code); outcome != OutcomeUnknown {
			return outcome
		}
		switch apiErr.StatusCode {
		case http.StatusNotFound:
			return OutcomeNotFound
		case http.StatusTooEarly:
			return OutcomeInitializing
		case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
			return OutcomeUnavailable
		case http.StatusTooManyRequests:
			return OutcomeRateLimit
		case http.StatusRequestTimeout:
			return OutcomeTimeout
		default:
			return OutcomeProtocolError
		}
	}
	return OutcomeUnknown
}

// ClassifySessionStatus maps an exact Omnigent lifecycle status to a stable
// fact without interpreting free-form messages.
func ClassifySessionStatus(status string) OutcomeKind {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "created", "ready", "idle", "running", "completed", "stopped":
		return OutcomeExists
	case "creating", "initializing", "starting":
		return OutcomeInitializing
	case "unavailable":
		return OutcomeUnavailable
	case "policy_pending", "waiting_for_policy":
		return OutcomePolicyPending
	case "canceled", "interrupted":
		return OutcomeCanceled
	case "timed_out", "timeout":
		return OutcomeTimeout
	case "exited", "failed":
		return OutcomeHarnessExit
	default:
		return OutcomeUnknown
	}
}

// ClassifyStreamEvent maps only machine-readable stream fields. Free-form
// event messages never affect the result.
func ClassifyStreamEvent(event StreamEvent) OutcomeKind {
	if event.Error != nil {
		if outcome := outcomeForCode(event.Error.Code); outcome != OutcomeUnknown {
			return outcome
		}
		if event.Error.Detail != nil {
			return ClassifyError(&APIError{StatusCode: event.Error.Detail.StatusCode})
		}
		return OutcomeProtocolError
	}
	if outcome := ClassifySessionStatus(event.Status); outcome != OutcomeUnknown {
		return outcome
	}
	switch strings.ToLower(strings.TrimSpace(event.Type)) {
	case "policy.request":
		return OutcomePolicyPending
	case "stream.closed", "attachment.lost":
		return OutcomeAttachmentLoss
	case "harness.exit":
		return OutcomeHarnessExit
	}
	return OutcomeUnknown
}

func outcomeForCode(code string) OutcomeKind {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "not_found", "conversation_not_found", "session_not_found":
		return OutcomeNotFound
	case "initializing", "session_initializing":
		return OutcomeInitializing
	case "unavailable", "backend_unavailable", "service_unavailable":
		return OutcomeUnavailable
	case "incompatible_version", "version_mismatch", "schema_mismatch":
		return OutcomeIncompatibleVersion
	case "invalid_profile", "unknown_profile", "profile_unavailable":
		return OutcomeInvalidProfile
	case "auth_failure", "authentication_failed", "invalid_api_key", "unauthorized":
		return OutcomeAuthFailure
	case "rate_limit", "rate_limited", "too_many_requests":
		return OutcomeRateLimit
	case "fallback_exhausted":
		return OutcomeFallbackExhausted
	case "policy_pending", "policy_required":
		return OutcomePolicyPending
	case "timeout", "timed_out":
		return OutcomeTimeout
	case "canceled", "interrupted":
		return OutcomeCanceled
	case "protocol_error", "invalid_response", "malformed_event":
		return OutcomeProtocolError
	case "harness_exit", "harness_failed":
		return OutcomeHarnessExit
	case "attachment_loss", "stream_closed":
		return OutcomeAttachmentLoss
	default:
		return OutcomeUnknown
	}
}
