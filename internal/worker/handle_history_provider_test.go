package worker

import (
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestSessionHandleHistoryProviderUsesPersistedProviderFamily(t *testing.T) {
	h := &SessionHandle{session: SessionSpec{Provider: "pi-litellm"}}
	info := sessionpkg.Info{
		Provider:     "pi-litellm",
		ProviderKind: "pi",
	}

	if got := h.historyProvider(info); got != "pi" {
		t.Fatalf("historyProvider() = %q, want persisted provider family %q", got, "pi")
	}
}
