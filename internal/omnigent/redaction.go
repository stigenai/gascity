package omnigent

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sync"
)

var sensitiveExternalText = regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?key|oauth[_-]?token|token|secret|password)\s*[:=]\s*\S+|\bsk-[A-Za-z0-9_-]{8,}|\bbearer\s+\S+|https?://[^/\s:@]+:[^@\s/]+@`)

func redactSensitiveText(value string) string {
	return sensitiveExternalText.ReplaceAllString(value, "[redacted]")
}

type redactingWriter struct {
	mu      sync.Mutex
	target  io.Writer
	pending []byte
}

func newRedactingWriter(target io.Writer) *redactingWriter {
	return &redactingWriter{target: target}
}

func (w *redactingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, data...)
	for {
		newline := bytes.IndexByte(w.pending, '\n')
		if newline < 0 {
			break
		}
		line := string(w.pending[:newline+1])
		w.pending = w.pending[newline+1:]
		if _, err := io.WriteString(w.target, redactSensitiveText(line)); err != nil {
			return len(data), err
		}
	}
	if len(w.pending) > maxSSEEventBytes {
		w.pending = w.pending[:0]
		if _, err := fmt.Fprintln(w.target, "[omnigent output redacted: line exceeds size limit]"); err != nil {
			return len(data), err
		}
	}
	return len(data), nil
}

func (w *redactingWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	_, err := io.WriteString(w.target, redactSensitiveText(string(w.pending)))
	w.pending = w.pending[:0]
	return err
}
