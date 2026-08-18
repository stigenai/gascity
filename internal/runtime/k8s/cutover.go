package k8s

import (
	"context"
	"errors"
	"strings"

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
	_ runtime.TerminalProvider                = (*seamBackedProvider)(nil)
	_ runtime.CapsuleStateRuntime             = (*seamBackedProvider)(nil)
	_ runtime.CapsuleStatePlace               = (*seamBackedProvider)(nil)
	_ runtime.CapsuleCityScopeProvider        = (*seamBackedProvider)(nil)
)

func (s *seamBackedProvider) CapsuleCityScope() string {
	return s.raw.CapsuleCityScope()
}

func (s *seamBackedProvider) EnsureCapsuleState(ctx context.Context, key runtime.CapsuleKey) (runtime.CapsuleStateReference, bool, error) {
	return s.raw.EnsureCapsuleState(ctx, key)
}

func (s *seamBackedProvider) OpenCapsuleState(ctx context.Context, key runtime.CapsuleKey) (runtime.CapsuleStateReference, bool, error) {
	return s.raw.OpenCapsuleState(ctx, key)
}

func (s *seamBackedProvider) ListCapsuleStates(ctx context.Context) ([]runtime.CapsuleStateReference, error) {
	return s.raw.ListCapsuleStates(ctx)
}

func (s *seamBackedProvider) PurgeCapsuleState(ctx context.Context, key runtime.CapsuleKey) error {
	return s.raw.PurgeCapsuleState(ctx, key)
}

func (s *seamBackedProvider) AttachCapsuleState(ctx context.Context, placeName string, ref runtime.CapsuleStateReference) error {
	return s.raw.AttachCapsuleState(ctx, placeName, ref)
}

func (s *seamBackedProvider) DetachCapsuleState(ctx context.Context, placeName string) error {
	return s.raw.DetachCapsuleState(ctx, placeName)
}

func (s *seamBackedProvider) ReadTerminal(ctx context.Context, name string, maxBytes int) (runtime.TerminalRead, error) {
	return s.raw.ReadTerminal(ctx, name, maxBytes)
}

func (s *seamBackedProvider) SendTerminalInput(ctx context.Context, name string, data []byte) error {
	return s.raw.SendTerminalInput(ctx, name, data)
}

func (s *seamBackedProvider) SendTerminalKeys(ctx context.Context, name string, keys ...string) error {
	return s.raw.SendTerminalKeys(ctx, name, keys...)
}

func (s *seamBackedProvider) ResizeTerminal(ctx context.Context, name string, size runtime.TerminalSize) error {
	return s.raw.ResizeTerminal(ctx, name, size)
}

func (s *seamBackedProvider) InterruptTerminal(ctx context.Context, name string) error {
	return s.raw.InterruptTerminal(ctx, name)
}

func (s *seamBackedProvider) DetachTerminal(ctx context.Context, name string) error {
	return s.raw.DetachTerminal(ctx, name)
}

// NewSeamBacked constructs a k8s provider served through the seams.
func NewSeamBacked() (runtime.Provider, error) {
	raw, err := NewProvider()
	if err != nil {
		return nil, err
	}
	return newSeamBacked(raw), nil
}

// NewSeamBackedForCity constructs the production provider with a stable
// Gas-City-owned capsule scope. It avoids requiring remote Kubernetes-specific
// configuration for identity that the local controller already knows.
func NewSeamBackedForCity(cityName string) (runtime.Provider, error) {
	raw, err := NewProvider()
	if err != nil {
		return nil, err
	}
	return newSeamBackedForCity(raw, cityName)
}

func newSeamBackedForCity(raw *Provider, cityName string) (runtime.Provider, error) {
	if raw == nil {
		return nil, errors.New("kubernetes provider is nil")
	}
	cityName = strings.TrimSpace(cityName)
	if cityName == "" {
		return nil, errors.New("kubernetes capsule city name is required")
	}
	cluster := strings.TrimSpace(raw.k8sContext)
	if cluster == "" {
		cluster = "in-cluster"
	}
	namespace := strings.TrimSpace(raw.namespace)
	if namespace == "" {
		return nil, errors.New("kubernetes capsule namespace is required")
	}
	raw.capsuleCityScope = strings.Join([]string{cluster, namespace, cityName}, "/")
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
