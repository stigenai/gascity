package api

import (
	"context"
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func (s *Server) capsuleStateControl() (*session.CapsuleStateControl, string, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, "", apierr.ServiceUnavailable.Msg("no session store configured")
	}
	provider := s.state.SessionProvider()
	if provider == nil {
		return nil, "", apierr.CapsuleStateUnsupported.Msg("session provider does not expose capsule state")
	}
	scope := strings.TrimSpace(s.state.CityName())
	if scoped, ok := provider.(runtime.CapsuleCityScopeProvider); ok {
		if providerScope := strings.TrimSpace(scoped.CapsuleCityScope()); providerScope != "" {
			scope = providerScope
		}
	}
	return session.NewCapsuleStateControl(store, provider), scope, nil
}

func capsuleStateReportOutput(report session.CapsuleStateReconcileReport, dryRun bool) *OmnigentCapsuleStateReportOutput {
	items := make([]OmnigentCapsuleStateItem, 0, len(report.Items))
	for _, item := range report.Items {
		items = append(items, OmnigentCapsuleStateItem{
			SessionID: item.SessionID,
			Action:    string(item.Action),
			Reason:    item.Reason,
		})
	}
	return &OmnigentCapsuleStateReportOutput{Body: OmnigentCapsuleStateReportBody{
		DryRun:         dryRun,
		Items:          items,
		IgnoredForeign: report.IgnoredForeign,
	}}
}

func capsuleStateAPIError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, session.ErrCapsuleStateControlUnsupported):
		return apierr.CapsuleStateUnsupported.Msg("session provider does not expose capsule state")
	case errors.Is(err, session.ErrCapsuleStateNotTracked):
		return apierr.SessionNotFound.Msg("session has no tracked capsule state")
	case errors.Is(err, session.ErrCapsuleStatePurgeNotTerminal),
		errors.Is(err, session.ErrCapsuleStatePurgeLive),
		errors.Is(err, runtime.ErrCapsuleStateConflict):
		return apierr.SessionConflict.Msg("capsule state purge safety check failed")
	default:
		// Provider errors may contain host names, pod UIDs, or filesystem paths.
		return apierr.ServiceUnavailable.Msg("capsule state operation failed")
	}
}

// humaHandleOmnigentCapsuleStateInspect returns a non-mutating provider and
// durable-session reconciliation report.
func (s *Server) humaHandleOmnigentCapsuleStateInspect(ctx context.Context, _ *OmnigentCapsuleStateInspectInput) (*OmnigentCapsuleStateReportOutput, error) {
	control, scope, err := s.capsuleStateControl()
	if err != nil {
		return nil, err
	}
	report, err := control.Inspect(ctx, scope)
	if err != nil {
		return nil, capsuleStateAPIError(err)
	}
	return capsuleStateReportOutput(report, true), nil
}

// humaHandleOmnigentCapsuleStatePurge previews or executes one explicit,
// durable, closed-session purge authorization.
func (s *Server) humaHandleOmnigentCapsuleStatePurge(ctx context.Context, input *OmnigentCapsuleStatePurgeInput) (*OmnigentCapsuleStateReportOutput, error) {
	control, scope, err := s.capsuleStateControl()
	if err != nil {
		return nil, err
	}
	report, err := control.Purge(ctx, scope, input.ID, input.Body.DryRun)
	if err != nil {
		return nil, capsuleStateAPIError(err)
	}
	return capsuleStateReportOutput(report, input.Body.DryRun), nil
}
