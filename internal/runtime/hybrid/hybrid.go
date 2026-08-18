// Package hybrid provides a composite [runtime.Provider] that routes
// operations to a local or remote backend based on session name.
package hybrid

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider routes session operations to a local or remote provider
// based on a name-matching function.
type Provider struct {
	local    runtime.Provider
	remote   runtime.Provider
	isRemote func(name string) bool
}

var (
	_ runtime.Provider                        = (*Provider)(nil)
	_ runtime.DeadRuntimeSessionChecker       = (*Provider)(nil)
	_ runtime.InteractionProvider             = (*Provider)(nil)
	_ runtime.InterruptBoundaryWaitProvider   = (*Provider)(nil)
	_ runtime.InterruptedTurnResetProvider    = (*Provider)(nil)
	_ runtime.RelaunchProvider                = (*Provider)(nil)
	_ runtime.LivenessObserver                = (*Provider)(nil)
	_ runtime.ServerLifecycleProvider         = (*Provider)(nil)
	_ runtime.FreshRunningSessionLister       = (*Provider)(nil)
	_ runtime.InstanceTokenFencedStopProvider = (*Provider)(nil)
	_ runtime.InstanceTokenFencedStopResolver = (*Provider)(nil)
	_ runtime.TerminalProvider                = (*Provider)(nil)
	_ runtime.CapsuleRouteResolver            = (*Provider)(nil)
	_ runtime.CapsuleStateRuntime             = (*Provider)(nil)
	_ runtime.CapsuleCityScopeProvider        = (*Provider)(nil)
)

func (p *Provider) terminal(name string) (runtime.TerminalProvider, error) {
	provider, ok := p.route(name).(runtime.TerminalProvider)
	if !ok {
		return nil, runtime.ErrTerminalUnsupported
	}
	return provider, nil
}

// ReadTerminal forwards a bounded snapshot request to the routed provider.
func (p *Provider) ReadTerminal(ctx context.Context, name string, maxBytes int) (runtime.TerminalRead, error) {
	provider, err := p.terminal(name)
	if err != nil {
		return runtime.TerminalRead{}, err
	}
	return provider.ReadTerminal(ctx, name, maxBytes)
}

// SendTerminalInput forwards literal input once to the routed provider.
func (p *Provider) SendTerminalInput(ctx context.Context, name string, data []byte) error {
	provider, err := p.terminal(name)
	if err != nil {
		return err
	}
	return provider.SendTerminalInput(ctx, name, data)
}

// SendTerminalKeys forwards logical keys once to the routed provider.
func (p *Provider) SendTerminalKeys(ctx context.Context, name string, keys ...string) error {
	provider, err := p.terminal(name)
	if err != nil {
		return err
	}
	return provider.SendTerminalKeys(ctx, name, keys...)
}

// ResizeTerminal forwards PTY geometry to the routed provider.
func (p *Provider) ResizeTerminal(ctx context.Context, name string, size runtime.TerminalSize) error {
	provider, err := p.terminal(name)
	if err != nil {
		return err
	}
	return provider.ResizeTerminal(ctx, name, size)
}

// InterruptTerminal forwards one soft interrupt to the routed provider.
func (p *Provider) InterruptTerminal(ctx context.Context, name string) error {
	provider, err := p.terminal(name)
	if err != nil {
		return err
	}
	return provider.InterruptTerminal(ctx, name)
}

// DetachTerminal releases routed attachment state without stopping the session.
func (p *Provider) DetachTerminal(ctx context.Context, name string) error {
	provider, err := p.terminal(name)
	if err != nil {
		return err
	}
	return provider.DetachTerminal(ctx, name)
}

// New creates a hybrid provider. isRemote returns true for sessions
// that should be managed by the remote provider.
func New(local, remote runtime.Provider, isRemote func(string) bool) *Provider {
	return &Provider{local: local, remote: remote, isRemote: isRemote}
}

func (p *Provider) route(name string) runtime.Provider {
	if p.isRemote(name) {
		return p.remote
	}
	return p.local
}

// ResolveCapsuleRoute reports the exact backend selected for sessionName and,
// for remote routes, returns its durable-state capability and stable scope.
func (p *Provider) ResolveCapsuleRoute(sessionName string) (bool, runtime.CapsuleStateRuntime, string, error) {
	if !p.isRemote(sessionName) {
		return false, nil, "", nil
	}
	state, ok := p.remote.(runtime.CapsuleStateRuntime)
	if !ok {
		return true, nil, "", fmt.Errorf("hybrid remote route for %q does not provide capsule state", sessionName)
	}
	var cityScope string
	if scoped, ok := p.remote.(runtime.CapsuleCityScopeProvider); ok {
		cityScope = scoped.CapsuleCityScope()
	}
	return true, state, cityScope, nil
}

func (p *Provider) capsuleStateRuntime() (runtime.CapsuleStateRuntime, error) {
	state, ok := p.remote.(runtime.CapsuleStateRuntime)
	if !ok {
		return nil, fmt.Errorf("hybrid remote runtime does not provide capsule state")
	}
	return state, nil
}

// CapsuleCityScope returns the remote backend's stable capsule scope. Local
// hybrid routes do not own capsule state.
func (p *Provider) CapsuleCityScope() string {
	if scoped, ok := p.remote.(runtime.CapsuleCityScopeProvider); ok {
		return scoped.CapsuleCityScope()
	}
	return ""
}

// EnsureCapsuleState forwards durable allocation to the remote backend.
func (p *Provider) EnsureCapsuleState(ctx context.Context, key runtime.CapsuleKey) (runtime.CapsuleStateReference, bool, error) {
	state, err := p.capsuleStateRuntime()
	if err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	return state.EnsureCapsuleState(ctx, key)
}

// OpenCapsuleState forwards exact allocation lookup to the remote backend.
func (p *Provider) OpenCapsuleState(ctx context.Context, key runtime.CapsuleKey) (runtime.CapsuleStateReference, bool, error) {
	state, err := p.capsuleStateRuntime()
	if err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	return state.OpenCapsuleState(ctx, key)
}

// ListCapsuleStates forwards provider-ground-truth inventory to the remote
// backend. Local hybrid routes never own capsule allocations.
func (p *Provider) ListCapsuleStates(ctx context.Context) ([]runtime.CapsuleStateReference, error) {
	state, err := p.capsuleStateRuntime()
	if err != nil {
		return nil, err
	}
	return state.ListCapsuleStates(ctx)
}

// PurgeCapsuleState forwards exact, ownership-fenced deletion to the remote
// backend.
func (p *Provider) PurgeCapsuleState(ctx context.Context, key runtime.CapsuleKey) error {
	state, err := p.capsuleStateRuntime()
	if err != nil {
		return err
	}
	return state.PurgeCapsuleState(ctx, key)
}

// ConfigureServer forwards shared local-server setup when the local backend
// owns one. The hybrid remote backend is Kubernetes and has no shared server.
func (p *Provider) ConfigureServer() error {
	if lifecycle, ok := p.local.(runtime.ServerLifecycleProvider); ok {
		return lifecycle.ConfigureServer()
	}
	return nil
}

// TeardownServer forwards shared local-server teardown after the hybrid
// provider's sessions have drained.
func (p *Provider) TeardownServer() error {
	if lifecycle, ok := p.local.(runtime.ServerLifecycleProvider); ok {
		return lifecycle.TeardownServer()
	}
	return nil
}

// Start delegates to the routed backend.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	return p.route(name).Start(ctx, name, cfg)
}

// Stop delegates to the routed backend.
func (p *Provider) Stop(name string) error {
	return p.route(name).Stop(name)
}

// StopIfInstanceToken preserves the routed backend's atomic identity fence.
// It never degrades to a separate metadata probe and name-based Stop.
func (p *Provider) StopIfInstanceToken(name, expectedToken string) error {
	provider, ok := p.ResolveInstanceTokenFencedStop(name)
	if !ok {
		return runtime.ErrFencedStopUnsupported
	}
	return provider.StopIfInstanceToken(name, expectedToken)
}

// ResolveInstanceTokenFencedStop resolves the optional atomic stop against the
// selected backend. A hybrid must not advertise its remote Kubernetes fence as
// proof that an unrelated local route is also safe.
func (p *Provider) ResolveInstanceTokenFencedStop(name string) (runtime.InstanceTokenFencedStopProvider, bool) {
	return runtime.ResolveInstanceTokenFencedStop(p.route(name), name)
}

// Interrupt delegates to the routed backend.
func (p *Provider) Interrupt(name string) error {
	return p.route(name).Interrupt(name)
}

// IsRunning delegates to the routed backend.
func (p *Provider) IsRunning(name string) bool {
	return p.route(name).IsRunning(name)
}

// IsDeadRuntimeSession delegates to the routed backend when it can positively
// distinguish live sessions from visible dead artifacts.
func (p *Provider) IsDeadRuntimeSession(name string) (bool, error) {
	checker, ok := p.route(name).(runtime.DeadRuntimeSessionChecker)
	if !ok {
		return false, nil
	}
	return checker.IsDeadRuntimeSession(name)
}

// IsAttached delegates to the routed backend.
func (p *Provider) IsAttached(name string) bool {
	return p.route(name).IsAttached(name)
}

// Attach delegates to the routed backend.
func (p *Provider) Attach(name string) error {
	return p.route(name).Attach(name)
}

// ProcessAlive delegates to the routed backend.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	return p.route(name).ProcessAlive(name, processNames)
}

// ObserveLiveness delegates to the routed backend through runtime.ObserveLiveness
// so the backend's native LivenessObserver fast-path is preserved (e.g. herdr's
// agent-status liveness) instead of collapsing to the generic
// IsRunning+ProcessAlive fold.
func (p *Provider) ObserveLiveness(name string, processNames []string) runtime.Liveness {
	return runtime.ObserveLiveness(p.route(name), name, processNames)
}

// Nudge delegates to the routed backend.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	return p.route(name).Nudge(name, content)
}

// WaitForIdle delegates to the routed backend when it supports explicit
// idle-boundary waiting.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	if wp, ok := p.route(name).(runtime.IdleWaitProvider); ok {
		return wp.WaitForIdle(ctx, name, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// NudgeNow delegates to the routed backend when it supports immediate
// injection without an internal wait-idle step.
func (p *Provider) NudgeNow(name string, content []runtime.ContentBlock) error {
	if np, ok := p.route(name).(runtime.ImmediateNudgeProvider); ok {
		return np.NudgeNow(name, content)
	}
	return p.route(name).Nudge(name, content)
}

// ResetInterruptedTurn delegates to the routed backend when it supports
// provider-native interrupted-turn discard semantics.
func (p *Provider) ResetInterruptedTurn(ctx context.Context, name string) error {
	if rp, ok := p.route(name).(runtime.InterruptedTurnResetProvider); ok {
		return rp.ResetInterruptedTurn(ctx, name)
	}
	return runtime.ErrInteractionUnsupported
}

// Relaunch forwards a warm-box agent relaunch to the routed backend when it
// supports one, so the reconciler's RelaunchProvider type-assert is not masked
// by the hybrid router.
func (p *Provider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	if rp, ok := p.route(name).(runtime.RelaunchProvider); ok {
		return rp.Relaunch(ctx, name, cfg)
	}
	return runtime.ErrRelaunchUnsupported
}

// WaitForInterruptBoundary delegates to the routed backend when it can confirm
// a provider-native interrupt boundary before the next turn is injected.
func (p *Provider) WaitForInterruptBoundary(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	if wp, ok := p.route(name).(runtime.InterruptBoundaryWaitProvider); ok {
		return wp.WaitForInterruptBoundary(ctx, name, since, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// Pending delegates to the routed backend when it supports structured
// interactions.
func (p *Provider) Pending(name string) (*runtime.PendingInteraction, error) {
	if ip, ok := p.route(name).(runtime.InteractionProvider); ok {
		return ip.Pending(name)
	}
	return nil, runtime.ErrInteractionUnsupported
}

// Respond delegates to the routed backend when it supports structured
// interactions.
func (p *Provider) Respond(name string, response runtime.InteractionResponse) error {
	if ip, ok := p.route(name).(runtime.InteractionProvider); ok {
		return ip.Respond(name, response)
	}
	return runtime.ErrInteractionUnsupported
}

// SetMeta delegates to the routed backend.
func (p *Provider) SetMeta(name, key, value string) error {
	return p.route(name).SetMeta(name, key, value)
}

// GetMeta delegates to the routed backend.
func (p *Provider) GetMeta(name, key string) (string, error) {
	return p.route(name).GetMeta(name, key)
}

// RemoveMeta delegates to the routed backend.
func (p *Provider) RemoveMeta(name, key string) error {
	return p.route(name).RemoveMeta(name, key)
}

// Peek delegates to the routed backend.
func (p *Provider) Peek(name string, lines int) (string, error) {
	return p.route(name).Peek(name, lines)
}

// ListRunning queries both backends and returns best-effort results plus a
// partial-list error when one backend fails.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	local, lErr := p.local.ListRunning(prefix)
	remote, rErr := p.remote.ListRunning(prefix)
	return runtime.MergeBackendListResults(
		runtime.BackendListResult{Label: "local", Names: local, Err: lErr},
		runtime.BackendListResult{Label: "remote", Names: remote, Err: rErr},
	)
}

// ListRunningFresh queries both backends through their uncached lifecycle
// inventory when available, preserving partial-list behavior.
func (p *Provider) ListRunningFresh(prefix string) ([]string, error) {
	local, lErr := runtime.ListRunningFresh(p.local, prefix)
	remote, rErr := runtime.ListRunningFresh(p.remote, prefix)
	return runtime.MergeBackendListResults(
		runtime.BackendListResult{Label: "local", Names: local, Err: lErr},
		runtime.BackendListResult{Label: "remote", Names: remote, Err: rErr},
	)
}

// GetLastActivity delegates to the routed backend.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	return p.route(name).GetLastActivity(name)
}

// ClearScrollback delegates to the routed backend.
func (p *Provider) ClearScrollback(name string) error {
	return p.route(name).ClearScrollback(name)
}

// CopyTo delegates to the routed backend.
func (p *Provider) CopyTo(name, src, relDst string) error {
	return p.route(name).CopyTo(name, src, relDst)
}

// SendKeys delegates to the routed backend.
func (p *Provider) SendKeys(name string, keys ...string) error {
	return p.route(name).SendKeys(name, keys...)
}

// RunLive delegates to the routed backend.
func (p *Provider) RunLive(name string, cfg runtime.Config) error {
	return p.route(name).RunLive(name, cfg)
}

// Capabilities returns the intersection of both backends' capabilities.
// A capability is reported only if both local and remote support it.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	lc := p.local.Capabilities()
	rc := p.remote.Capabilities()
	return runtime.ProviderCapabilities{
		CanReportAttachment: lc.CanReportAttachment && rc.CanReportAttachment,
		CanReportActivity:   lc.CanReportActivity && rc.CanReportActivity,
	}
}

// SleepCapability reports idle sleep capability for the routed backend.
func (p *Provider) SleepCapability(name string) runtime.SessionSleepCapability {
	if scp, ok := p.route(name).(runtime.SleepCapabilityProvider); ok {
		return scp.SleepCapability(name)
	}
	return runtime.SessionSleepCapabilityDisabled
}
