package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type providerSessionResponseHandle struct {
	provider     runtime.Provider
	sessionName  string
	providerName string
}

func newProviderSessionResponseHandle(sp runtime.Provider, sessionName, providerName string) sessionResponseHandle {
	sessionName = strings.TrimSpace(sessionName)
	if sp == nil || sessionName == "" {
		return nil
	}
	return providerSessionResponseHandle{
		provider:     sp,
		sessionName:  sessionName,
		providerName: strings.TrimSpace(providerName),
	}
}

func (h providerSessionResponseHandle) State(context.Context) (worker.State, error) {
	state := worker.State{
		SessionName: h.sessionName,
		Provider:    h.providerName,
	}
	if h.provider == nil || !h.provider.IsRunning(h.sessionName) {
		state.Phase = worker.PhaseStopped
		return state, nil
	}
	state.Phase = worker.PhaseReady
	return state, nil
}

func (h providerSessionResponseHandle) Peek(_ context.Context, lines int) (string, error) {
	if h.provider == nil || !h.provider.IsRunning(h.sessionName) {
		return "", fmt.Errorf("%w: %s", session.ErrSessionInactive, h.sessionName)
	}
	return h.provider.Peek(h.sessionName, lines)
}
