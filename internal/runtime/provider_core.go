package runtime

import (
	"errors"
	"fmt"
)

// MetaProviderSessionID is the runtime metadata key for a provider-native
// conversation identifier that can resume and address the current session.
const MetaProviderSessionID = "GC_PROVIDER_SESSION_ID"

// PartialListError reports that ListRunning returned best-effort results while
// one or more backends failed. Callers may continue using the returned names
// slice, but should surface the degraded backend error to operators.
type PartialListError struct {
	Err error
}

// Error returns the aggregated backend failure message.
func (e *PartialListError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// Unwrap exposes the aggregated backend failure.
func (e *PartialListError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// BackendError carries provider/backend context for aggregated failures.
type BackendError struct {
	Label string
	Err   error
}

// BackendListResult captures one backend's ListRunning result.
type BackendListResult struct {
	Label string
	Names []string
	Err   error
}

// IsPartialListError reports whether err represents a degraded-but-usable
// ListRunning result from one or more failed backends.
func IsPartialListError(err error) bool {
	var target *PartialListError
	return errors.As(err, &target)
}

// DeadRuntimeSessionChecker is an optional provider capability for destructive
// cleanup paths that need positive proof a visible runtime artifact is dead.
// A false result means either the session is live, absent, or unsupported by
// the backend; a non-nil error means liveness could not be confirmed.
type DeadRuntimeSessionChecker interface {
	// IsDeadRuntimeSession reports whether name is visible but confirmed dead.
	IsDeadRuntimeSession(name string) (bool, error)
}

// ProcessAliveChecker is an optional provider capability for destructive
// remediation paths (e.g. doctor's zombie-session cleanup) that need to tell
// a confirmed-dead process apart from a liveness probe that could not
// complete, such as an API timeout or transport failure. Provider's
// ProcessAlive is the best-effort, error-swallowing sibling every provider
// implements; callers that would otherwise treat "could not confirm" as
// proof of death should prefer this checked variant when the concrete
// provider implements it.
type ProcessAliveChecker interface {
	// ProcessAliveChecked reports whether any of processNames is running in
	// the named session. A non-nil error means liveness could not be
	// confirmed either way and must not be treated as a confirmed death.
	ProcessAliveChecked(name string, processNames []string) (bool, error)
}

// RunningChecker is the IsRunning analog of ProcessAliveChecker: an
// optional provider capability for callers that need to tell a confirmed
// "not running" apart from a liveness probe that could not complete, such
// as an API timeout or transport failure. Provider's IsRunning is the
// best-effort, error-swallowing sibling every provider implements; callers
// that would otherwise treat "could not confirm" as a confirmed negative
// should prefer this checked variant when the concrete provider implements
// it.
type RunningChecker interface {
	// IsRunningChecked reports whether name has a running session. A
	// non-nil error means running state could not be confirmed either way
	// and must not be treated as a confirmed negative.
	IsRunningChecked(name string) (bool, error)
}

// AttachedChecker is the IsAttached analog of RunningChecker.
type AttachedChecker interface {
	// IsAttachedChecked reports whether a user terminal is attached to
	// name's session. A non-nil error means attachment could not be
	// confirmed either way and must not be treated as a confirmed negative.
	IsAttachedChecked(name string) (bool, error)
}

// ProcessAliveChecked reports checked process liveness via ProcessAliveChecker
// when provider implements it, and otherwise falls back to the best-effort
// Provider.ProcessAlive with a nil error.
func ProcessAliveChecked(provider Provider, name string, processNames []string) (bool, error) {
	if checker, ok := provider.(ProcessAliveChecker); ok {
		return checker.ProcessAliveChecked(name, processNames)
	}
	return provider.ProcessAlive(name, processNames), nil
}

// IsRunningChecked reports checked running state via RunningChecker when
// provider implements it, and otherwise falls back to the best-effort
// Provider.IsRunning with a nil error.
func IsRunningChecked(provider Provider, name string) (bool, error) {
	if checker, ok := provider.(RunningChecker); ok {
		return checker.IsRunningChecked(name)
	}
	return provider.IsRunning(name), nil
}

// IsAttachedChecked reports checked attachment state via AttachedChecker
// when provider implements it, and otherwise falls back to the best-effort
// Provider.IsAttached with a nil error.
func IsAttachedChecked(provider Provider, name string) (bool, error) {
	if checker, ok := provider.(AttachedChecker); ok {
		return checker.IsAttachedChecked(name)
	}
	return provider.IsAttached(name), nil
}

// MergeBackendListResults merges provider ListRunning results. On partial
// backend failure it returns the best-effort merged names plus a
// [PartialListError] so callers can continue with partial results while still
// surfacing backend degradation. Only a total failure returns no names.
func MergeBackendListResults(results ...BackendListResult) ([]string, error) {
	merged := make([]string, 0)
	failures := make([]error, 0, len(results))
	failed := 0

	for _, result := range results {
		merged = append(merged, result.Names...)
	}

	for _, result := range results {
		if result.Err == nil {
			continue
		}
		failed++
		failures = append(failures, fmt.Errorf("%s backend: %w", result.Label, result.Err))
	}

	if len(failures) == 0 {
		return merged, nil
	}
	if len(merged) > 0 {
		return merged, &PartialListError{Err: errors.Join(failures...)}
	}
	if failed == len(results) {
		return nil, errors.Join(failures...)
	}
	return merged, &PartialListError{Err: errors.Join(failures...)}
}

// MergeBackendStopErrors standardizes multi-backend Stop semantics.
// Any successful stop wins. If every backend reports the session as gone,
// Stop remains idempotent and returns nil.
func MergeBackendStopErrors(results ...BackendError) error {
	failures := make([]error, 0, len(results))
	allGone := len(results) > 0

	for _, result := range results {
		if result.Err == nil {
			return nil
		}
		if !IsSessionGone(result.Err) {
			allGone = false
		}
		failures = append(failures, fmt.Errorf("%s backend: %w", result.Label, result.Err))
	}

	if len(failures) == 0 || allGone {
		return nil
	}
	return errors.Join(failures...)
}
