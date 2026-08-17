package api

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// SessionTerminalSnapshot is a bounded, reconnectable terminal image. Cursor
// is opaque and can be supplied on the next read to avoid retransmitting an
// unchanged image.
type SessionTerminalSnapshot struct {
	SessionID string
	Cursor    string
	Data      []byte
	Truncated bool
	Unchanged bool
}

// ReadSessionTerminal reads a lifecycle-neutral terminal snapshot.
func (c *Client) ReadSessionTerminal(ctx context.Context, id string, maxBytes int, ifSnapshot string) (SessionTerminalSnapshot, error) {
	if err := c.requireCityScope(); err != nil {
		return SessionTerminalSnapshot{}, err
	}
	limit := int64(maxBytes)
	params := &genclient.GetV0CityByCityNameSessionByIdTerminalParams{MaxBytes: &limit}
	if ifSnapshot != "" {
		params.IfSnapshot = &ifSnapshot
	}
	resp, err := c.cw.GetV0CityByCityNameSessionByIdTerminalWithResponse(ctx, c.cityName, id, params)
	if err != nil {
		return SessionTerminalSnapshot{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return SessionTerminalSnapshot{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return SessionTerminalSnapshot{}, err
	}
	if resp.JSON200 == nil {
		return SessionTerminalSnapshot{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	out := SessionTerminalSnapshot{
		SessionID: resp.JSON200.SessionId, Cursor: resp.JSON200.Cursor,
		Truncated: resp.JSON200.Truncated, Unchanged: resp.JSON200.Unchanged,
	}
	if resp.JSON200.Data != nil {
		out.Data, err = base64.StdEncoding.DecodeString(*resp.JSON200.Data)
		if err != nil {
			return SessionTerminalSnapshot{}, fmt.Errorf("decode terminal snapshot: %w", err)
		}
	}
	return out, nil
}

// SendSessionTerminalInput forwards literal terminal bytes exactly once.
func (c *Client) SendSessionTerminalInput(ctx context.Context, id string, data []byte) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PostV0CityByCityNameSessionByIdTerminalInputWithResponse(
		ctx, c.cityName, id,
		&genclient.PostV0CityByCityNameSessionByIdTerminalInputParams{XGCRequest: "true"},
		genclient.SessionTerminalInputBody{Data: base64.StdEncoding.EncodeToString(data)},
	)
	return terminalMutationError(resp, err)
}

// SendSessionTerminalKeys forwards logical terminal keys exactly once.
func (c *Client) SendSessionTerminalKeys(ctx context.Context, id string, keys ...string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	values := append([]string(nil), keys...)
	resp, err := c.cw.PostV0CityByCityNameSessionByIdTerminalKeysWithResponse(
		ctx, c.cityName, id,
		&genclient.PostV0CityByCityNameSessionByIdTerminalKeysParams{XGCRequest: "true"},
		genclient.SessionTerminalKeysBody{Keys: &values},
	)
	return terminalMutationError(resp, err)
}

// ResizeSessionTerminal forwards the authoritative terminal geometry.
func (c *Client) ResizeSessionTerminal(ctx context.Context, id string, rows, columns int) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PostV0CityByCityNameSessionByIdTerminalResizeWithResponse(
		ctx, c.cityName, id,
		&genclient.PostV0CityByCityNameSessionByIdTerminalResizeParams{XGCRequest: "true"},
		genclient.SessionTerminalResizeBody{Rows: int64(rows), Columns: int64(columns)},
	)
	return terminalMutationError(resp, err)
}

// InterruptSessionTerminal forwards one soft interrupt.
func (c *Client) InterruptSessionTerminal(ctx context.Context, id string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PostV0CityByCityNameSessionByIdTerminalInterruptWithResponse(
		ctx, c.cityName, id,
		&genclient.PostV0CityByCityNameSessionByIdTerminalInterruptParams{XGCRequest: "true"},
	)
	return terminalMutationError(resp, err)
}

// DetachSessionTerminal releases transient attachment state without stopping
// or otherwise mutating the worker lifecycle.
func (c *Client) DetachSessionTerminal(ctx context.Context, id string) error {
	if err := c.requireCityScope(); err != nil {
		return err
	}
	resp, err := c.cw.PostV0CityByCityNameSessionByIdTerminalDetachWithResponse(
		ctx, c.cityName, id,
		&genclient.PostV0CityByCityNameSessionByIdTerminalDetachParams{XGCRequest: "true"},
	)
	return terminalMutationError(resp, err)
}

func terminalMutationError(resp any, transportErr error) error {
	if transportErr != nil {
		return &connError{err: fmt.Errorf("request failed: %w", transportErr)}
	}
	if resp == nil {
		return &connError{err: fmt.Errorf("nil response")}
	}
	typed, ok := resp.(interface{ StatusCode() int })
	if !ok {
		return fmt.Errorf("invalid terminal response")
	}
	return apiErrorFromResponse(typed.StatusCode(), pdOf(resp))
}
