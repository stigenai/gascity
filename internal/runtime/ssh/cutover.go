package ssh

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing SleepCapability
// through to the underlying *Provider. The early cut-over for the ssh provider.
//
// ExecProvider is not passed through: ssh's Exec (over the connection) is what
// the carrier drives over internally; no production caller type-asserts it.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider = (*seamBackedProvider)(nil)
	_ runtime.RelaunchProvider        = (*seamBackedProvider)(nil)
	_ runtime.CapsuleStateRuntime     = (*seamBackedProvider)(nil)
	_ runtime.TerminalProvider        = (*seamBackedProvider)(nil)
)

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

// NewSeamBacked constructs an ssh provider for ep served through the seams.
func NewSeamBacked(ep Endpoint) runtime.Provider {
	raw := NewProvider(ep)
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// Relaunch passes through to the underlying provider's warm-box relaunch
// (respawn-pane over the ssh Conn; B2, RelaunchProvider).
func (s *seamBackedProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	return s.raw.Relaunch(ctx, name, cfg)
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
