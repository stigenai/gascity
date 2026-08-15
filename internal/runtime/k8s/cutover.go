package k8s

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing SleepCapability
// through to the underlying *Provider. The early cut-over for the k8s provider.
//
// ExecProvider is not passed through: k8s's Exec (execInPod) is the connection
// the carrier drives over internally; no production caller type-asserts it.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                        = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider         = (*seamBackedProvider)(nil)
	_ runtime.RelaunchProvider                = (*seamBackedProvider)(nil)
	_ runtime.FreshRunningSessionLister       = (*seamBackedProvider)(nil)
	_ runtime.InstanceTokenFencedStopProvider = (*seamBackedProvider)(nil)
	_ runtime.RunningChecker                  = (*seamBackedProvider)(nil)
	_ runtime.AttachedChecker                 = (*seamBackedProvider)(nil)
	_ runtime.ProcessAliveChecker             = (*seamBackedProvider)(nil)
)

// NewSeamBacked constructs a k8s provider served through the seams.
func NewSeamBacked() (runtime.Provider, error) {
	raw, err := NewProvider()
	if err != nil {
		return nil, err
	}
	return newSeamBacked(raw), nil
}

// newSeamBacked composes the production provider while keeping construction
// injectable for contract tests.
func newSeamBacked(raw *Provider) runtime.Provider {
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// Relaunch passes through to the underlying provider's warm-pod relaunch
// (respawn-pane via execInPod; B2, RelaunchProvider).
func (s *seamBackedProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	return s.raw.Relaunch(ctx, name, cfg)
}

// ListRunningFresh preserves the raw provider's uncached lifecycle inventory
// across the production seam wrapper.
func (s *seamBackedProvider) ListRunningFresh(prefix string) ([]string, error) {
	return s.raw.ListRunningFresh(prefix)
}

func (s *seamBackedProvider) StopIfInstanceToken(name, expectedToken string) error {
	return s.raw.StopIfInstanceToken(name, expectedToken)
}

// The generic seam adapter preserves legacy best-effort behavior when
// attachment lookup fails. Kubernetes can distinguish an absent pod from an
// unavailable API, so route these session-scoped reads and writes through the
// raw provider to preserve its typed errors.
func (s *seamBackedProvider) Nudge(name string, content []runtime.ContentBlock) error {
	return s.raw.Nudge(name, content)
}

func (s *seamBackedProvider) SendKeys(name string, keys ...string) error {
	return s.raw.SendKeys(name, keys...)
}

func (s *seamBackedProvider) SetMeta(name, key, value string) error {
	return s.raw.SetMeta(name, key, value)
}

func (s *seamBackedProvider) GetMeta(name, key string) (string, error) {
	return s.raw.GetMeta(name, key)
}

func (s *seamBackedProvider) RemoveMeta(name, key string) error {
	return s.raw.RemoveMeta(name, key)
}

func (s *seamBackedProvider) Peek(name string, lines int) (string, error) {
	return s.raw.Peek(name, lines)
}

// The generic seam adapter's IsRunning/IsAttached/ProcessAlive collapse any
// Open/Observe error (including an inconclusive API timeout) into a
// confirmed negative — exactly the collapse PR #69 (gcy-2sh/gcy-rfn) added
// the checked variants to eliminate. Route them through the raw provider,
// which already distinguishes a confirmed negative from an inconclusive
// probe, so callers using runtime.IsRunningChecked/IsAttachedChecked/
// ProcessAliveChecked for destructive remediation get that distinction
// through the seam wrapper too (gcy-envb).
func (s *seamBackedProvider) IsRunningChecked(name string) (bool, error) {
	return s.raw.IsRunningChecked(name)
}

func (s *seamBackedProvider) IsAttachedChecked(name string) (bool, error) {
	return s.raw.IsAttachedChecked(name)
}

func (s *seamBackedProvider) ProcessAliveChecked(name string, processNames []string) (bool, error) {
	return s.raw.ProcessAliveChecked(name, processNames)
}
