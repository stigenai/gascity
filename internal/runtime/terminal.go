package runtime

import (
	"context"
	"errors"
)

// ErrTerminalUnsupported reports that a provider cannot expose the generic
// remote terminal surface for the requested session.
var ErrTerminalUnsupported = errors.New("remote terminal attachment is unsupported")

// TerminalSize is the requested PTY geometry.
type TerminalSize struct {
	Rows    int
	Columns int
}

// TerminalRead is one bounded terminal snapshot. Truncated means the provider
// returned only the newest MaxBytes bytes requested by the caller.
type TerminalRead struct {
	Data      []byte
	Truncated bool
}

// TerminalProvider is the optional, provider-neutral remote terminal surface.
// Implementations must execute each mutation at most once: an ambiguous
// transport failure is returned to the caller and never retried internally.
// Detach releases only transient attachment state and must never stop the
// underlying session.
type TerminalProvider interface {
	ReadTerminal(ctx context.Context, name string, maxBytes int) (TerminalRead, error)
	SendTerminalInput(ctx context.Context, name string, data []byte) error
	SendTerminalKeys(ctx context.Context, name string, keys ...string) error
	ResizeTerminal(ctx context.Context, name string, size TerminalSize) error
	InterruptTerminal(ctx context.Context, name string) error
	DetachTerminal(ctx context.Context, name string) error
}

// TerminalCarrier adds literal input and PTY resizing to the high-level
// Carrier verbs used by tmux-in-a-box providers.
type TerminalCarrier interface {
	SendText(ctx context.Context, name string, data []byte) error
	Resize(ctx context.Context, name string, size TerminalSize) error
}

// BoundTerminalRead keeps the newest maxBytes bytes of data.
func BoundTerminalRead(data []byte, maxBytes int) TerminalRead {
	if maxBytes < 0 {
		maxBytes = 0
	}
	truncated := len(data) > maxBytes
	if truncated {
		data = data[len(data)-maxBytes:]
	}
	return TerminalRead{Data: append([]byte(nil), data...), Truncated: truncated}
}
