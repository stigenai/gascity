package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/runtime"
)

func (s *Server) resolveSessionTerminal(inputID string) (string, string, runtime.TerminalProvider, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return "", "", nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	id, err := s.resolveSessionIDWithConfig(store.Store, inputID)
	if err != nil {
		return "", "", nil, humaResolveError(err)
	}
	info, err := s.sessionManager(store.Store).Get(id)
	if err != nil {
		return "", "", nil, humaSessionManagerError(err)
	}
	name := strings.TrimSpace(info.SessionName)
	if name == "" {
		return "", "", nil, apierr.SessionNotFound.Msg("session has no live runtime")
	}
	provider, ok := s.state.SessionProvider().(runtime.TerminalProvider)
	if !ok {
		return "", "", nil, apierr.TerminalUnsupported.Msg("session provider does not support remote terminal attachment")
	}
	return id, name, provider, nil
}

func terminalAPIError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch {
	case errors.Is(err, runtime.ErrTerminalUnsupported):
		return apierr.TerminalUnsupported.Msg("session provider does not support the requested terminal action")
	case errors.Is(err, runtime.ErrSessionNotFound):
		return apierr.SessionNotFound.Msg("session runtime is not available")
	default:
		// Provider errors can contain SSH endpoints, pod names, filesystem paths,
		// or credentials. Keep them server-side; the remote wire is deliberately
		// generic and machine-branchable.
		return apierr.ServiceUnavailable.Msg("terminal operation failed")
	}
}

// humaHandleSessionTerminalSnapshot returns a bounded, reconnectable terminal snapshot.
func (s *Server) humaHandleSessionTerminalSnapshot(ctx context.Context, input *SessionTerminalSnapshotInput) (*SessionTerminalSnapshotOutput, error) {
	id, name, provider, err := s.resolveSessionTerminal(input.ID)
	if err != nil {
		return nil, err
	}
	maxBytes := input.MaxBytes
	if maxBytes < 0 || maxBytes > maxTerminalReadBytes {
		return nil, apierr.ValidationFailed.Msg("max_bytes must be between 0 and 262144")
	}
	if maxBytes == 0 {
		maxBytes = defaultTerminalReadBytes
	}
	read, err := provider.ReadTerminal(ctx, name, maxBytes)
	if err != nil {
		return nil, terminalAPIError(err)
	}
	providerTruncated := read.Truncated
	read = runtime.BoundTerminalRead(read.Data, maxBytes)
	read.Truncated = read.Truncated || providerTruncated
	sum := sha256.Sum256(read.Data)
	cursor := hex.EncodeToString(sum[:])
	unchanged := input.IfSnapshot != "" && input.IfSnapshot == cursor
	out := &SessionTerminalSnapshotOutput{}
	out.Body = SessionTerminalSnapshotBody{
		SessionID: id, Cursor: cursor, Truncated: read.Truncated, Unchanged: unchanged,
	}
	if !unchanged {
		out.Body.Data = read.Data
	}
	return out, nil
}

func terminalActionOutput(id, action string) *SessionTerminalActionOutput {
	out := &SessionTerminalActionOutput{}
	out.Body = SessionTerminalActionBody{Status: "ok", SessionID: id, Action: action}
	return out
}

// humaHandleSessionTerminalInput forwards literal terminal bytes exactly once.
func (s *Server) humaHandleSessionTerminalInput(ctx context.Context, input *SessionTerminalInput) (*SessionTerminalActionOutput, error) {
	if len(input.Body.Data) == 0 || len(input.Body.Data) > maxTerminalInputBytes {
		return nil, apierr.ValidationFailed.Msg("terminal input must contain between 1 and 65536 decoded bytes")
	}
	if strings.IndexByte(string(input.Body.Data), 0) >= 0 {
		return nil, apierr.ValidationFailed.Msg("terminal input cannot contain NUL")
	}
	id, name, provider, err := s.resolveSessionTerminal(input.ID)
	if err != nil {
		return nil, err
	}
	if err := provider.SendTerminalInput(ctx, name, input.Body.Data); err != nil {
		return nil, terminalAPIError(err)
	}
	return terminalActionOutput(id, "input"), nil
}

// humaHandleSessionTerminalKeys forwards bounded logical keys exactly once.
func (s *Server) humaHandleSessionTerminalKeys(ctx context.Context, input *SessionTerminalKeysInput) (*SessionTerminalActionOutput, error) {
	if len(input.Body.Keys) == 0 || len(input.Body.Keys) > maxTerminalKeys {
		return nil, apierr.ValidationFailed.Msg("terminal keys must contain between 1 and 32 entries")
	}
	for _, key := range input.Body.Keys {
		if strings.TrimSpace(key) == "" || len(key) > maxTerminalKeyBytes || strings.ContainsRune(key, 0) {
			return nil, apierr.ValidationFailed.Msg("terminal keys must be non-empty and at most 64 bytes")
		}
	}
	id, name, provider, err := s.resolveSessionTerminal(input.ID)
	if err != nil {
		return nil, err
	}
	if err := provider.SendTerminalKeys(ctx, name, input.Body.Keys...); err != nil {
		return nil, terminalAPIError(err)
	}
	return terminalActionOutput(id, "keys"), nil
}

// humaHandleSessionTerminalResize changes the authoritative terminal geometry.
func (s *Server) humaHandleSessionTerminalResize(ctx context.Context, input *SessionTerminalResizeInput) (*SessionTerminalActionOutput, error) {
	if input.Body.Rows < 1 || input.Body.Rows > maxTerminalDimension || input.Body.Columns < 1 || input.Body.Columns > maxTerminalDimension {
		return nil, apierr.ValidationFailed.Msg("terminal rows and columns must be between 1 and 1000")
	}
	id, name, provider, err := s.resolveSessionTerminal(input.ID)
	if err != nil {
		return nil, err
	}
	if err := provider.ResizeTerminal(ctx, name, runtime.TerminalSize{Rows: input.Body.Rows, Columns: input.Body.Columns}); err != nil {
		return nil, terminalAPIError(err)
	}
	return terminalActionOutput(id, "resize"), nil
}

// humaHandleSessionTerminalInterrupt sends one soft interrupt.
func (s *Server) humaHandleSessionTerminalInterrupt(ctx context.Context, input *SessionIDInput) (*SessionTerminalActionOutput, error) {
	id, name, provider, err := s.resolveSessionTerminal(input.ID)
	if err != nil {
		return nil, err
	}
	if err := provider.InterruptTerminal(ctx, name); err != nil {
		return nil, terminalAPIError(err)
	}
	return terminalActionOutput(id, "interrupt"), nil
}

// humaHandleSessionTerminalDetach cleanly releases transient attachment state
// without changing the worker lifecycle.
func (s *Server) humaHandleSessionTerminalDetach(ctx context.Context, input *SessionIDInput) (*SessionTerminalActionOutput, error) {
	id, name, provider, err := s.resolveSessionTerminal(input.ID)
	if err != nil {
		return nil, err
	}
	if err := provider.DetachTerminal(ctx, name); err != nil {
		return nil, terminalAPIError(err)
	}
	return terminalActionOutput(id, "detach"), nil
}
