package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"golang.org/x/term"
)

const remoteTerminalPollInterval = 150 * time.Millisecond

type remoteTerminalClient interface {
	ReadSessionTerminal(context.Context, string, int, string) (api.SessionTerminalSnapshot, error)
	SendSessionTerminalInput(context.Context, string, []byte) error
	ResizeSessionTerminal(context.Context, string, int, int) error
	DetachSessionTerminal(context.Context, string) error
}

func runRemoteSessionTerminal(ctx context.Context, client remoteTerminalClient, sessionID string, stdout, stderr io.Writer) error {
	stdin := io.Reader(os.Stdin)
	stdinFile, stdinTTY := os.Stdin, term.IsTerminal(int(os.Stdin.Fd()))
	stdoutFile, stdoutTTY := stdout.(*os.File)
	isTTY := stdinTTY && stdoutTTY && term.IsTerminal(int(stdoutFile.Fd()))
	var restore func() error
	if isTTY {
		state, err := term.MakeRaw(int(stdinFile.Fd()))
		if err != nil {
			return fmt.Errorf("enter raw terminal mode: %w", err)
		}
		restore = func() error { return term.Restore(int(stdinFile.Fd()), state) }
		defer func() { _ = restore() }()
	}
	return streamRemoteSessionTerminal(ctx, client, sessionID, stdin, stdout, stderr, isTTY, func() (int, int, error) {
		if !stdoutTTY {
			return 0, 0, errors.New("stdout is not a terminal")
		}
		columns, rows, err := term.GetSize(int(stdoutFile.Fd()))
		return rows, columns, err
	})
}

func streamRemoteSessionTerminal(
	ctx context.Context,
	client remoteTerminalClient,
	sessionID string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	isTTY bool,
	terminalSize func() (rows, columns int, err error),
) (returnErr error) {
	if client == nil {
		return errors.New("remote terminal client is unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		detachCtx, detachCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer detachCancel()
		if err := client.DetachSessionTerminal(detachCtx, sessionID); err != nil {
			fmt.Fprintf(stderr, "gc session attach: detach remote terminal: %v\n", err) //nolint:errcheck
		}
	}()

	resize := func() error {
		if !isTTY || terminalSize == nil {
			return nil
		}
		rows, columns, err := terminalSize()
		if err != nil {
			return fmt.Errorf("read terminal size: %w", err)
		}
		if err := client.ResizeSessionTerminal(ctx, sessionID, rows, columns); err != nil {
			return fmt.Errorf("resize remote terminal: %w", err)
		}
		return nil
	}
	if err := resize(); err != nil {
		return err
	}

	var cursor string
	render := func() error {
		snapshot, err := client.ReadSessionTerminal(ctx, sessionID, 256<<10, cursor)
		if err != nil {
			return fmt.Errorf("read remote terminal: %w", err)
		}
		cursor = snapshot.Cursor
		if snapshot.Unchanged {
			return nil
		}
		if isTTY {
			if _, err := io.WriteString(stdout, "\x1b[H\x1b[2J"); err != nil {
				return err
			}
		}
		if _, err := stdout.Write(snapshot.Data); err != nil {
			return fmt.Errorf("render remote terminal: %w", err)
		}
		return nil
	}
	if err := render(); err != nil {
		return err
	}

	type inputResult struct {
		data []byte
		err  error
	}
	inputCh := make(chan inputResult, 1)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := stdin.Read(buf)
			data := append([]byte(nil), buf[:n]...)
			select {
			case inputCh <- inputResult{data: data, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	resizeCh := make(chan os.Signal, 1)
	if isTTY {
		signal.Notify(resizeCh, syscall.SIGWINCH)
		defer signal.Stop(resizeCh)
	}
	ticker := time.NewTicker(remoteTerminalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resizeCh:
			if err := resize(); err != nil {
				return err
			}
		case input := <-inputCh:
			if len(input.data) > 0 {
				if err := client.SendSessionTerminalInput(ctx, sessionID, input.data); err != nil {
					return fmt.Errorf("write remote terminal: %w", err)
				}
			}
			if input.err != nil {
				if errors.Is(input.err, io.EOF) {
					return nil
				}
				return fmt.Errorf("read local terminal: %w", input.err)
			}
		case <-ticker.C:
			if err := render(); err != nil {
				return err
			}
		}
	}
}
