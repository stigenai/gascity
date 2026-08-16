package omnigent

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
)

var sensitiveExternalText = regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?key|oauth[_-]?token|token|secret|password)\s*[:=]\s*\S+|\bsk-[A-Za-z0-9_-]{8,}|\bbearer\s+\S+|https?://[^/\s:@]+:[^@\s/]+@`)

func redactSensitiveText(value string) string {
	return sensitiveExternalText.ReplaceAllString(value, "[redacted]")
}

// remoteRedactor removes values and addressing metadata that are allowed only
// at the capsule/provider boundary. Local diagnostics deliberately remain
// separate because they may name missing environment variables and local paths.
type remoteRedactor struct {
	environment []string
	literals    []string
}

func newRemoteRedactor(environment []string, references []runtime.SecretReference, extra ...string) remoteRedactor {
	redactor := remoteRedactor{environment: append([]string(nil), environment...)}
	seen := make(map[string]bool)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if len(value) < 4 || seen[value] {
			return
		}
		seen[value] = true
		redactor.literals = append(redactor.literals, value)
	}
	environmentNames := make(map[string]bool)
	for _, ref := range references {
		add(ref.Environment)
		environmentNames[ref.Environment] = ref.Environment != ""
		add(ref.MountPath)
		if ref.Kubernetes != nil {
			add(ref.Kubernetes.Name)
			add(ref.Kubernetes.Key)
		}
		if ref.SSH != nil {
			add(ref.SSH.Path)
		}
	}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok && (environmentNames[key] || execenv.IsSensitiveKey(key)) {
			add(value)
		}
	}
	for _, value := range extra {
		add(value)
	}
	sort.Slice(redactor.literals, func(i, j int) bool { return len(redactor.literals[i]) > len(redactor.literals[j]) })
	return redactor
}

func (r remoteRedactor) Redact(value string) string {
	value = execenv.RedactText(redactSensitiveText(value), r.environment)
	for _, literal := range r.literals {
		value = strings.ReplaceAll(value, literal, execenv.Redacted)
	}
	return value
}

type remoteRedactedError struct {
	message string
	cause   error
}

func (e *remoteRedactedError) Error() string { return e.message }
func (e *remoteRedactedError) Unwrap() error { return e.cause }

func redactRemoteError(err error, redactor remoteRedactor) error {
	if err == nil {
		return nil
	}
	return &remoteRedactedError{message: redactor.Redact(err.Error()), cause: err}
}

type redactingWriter struct {
	mu      sync.Mutex
	target  io.Writer
	pending []byte
	redact  func(string) string
}

func newRedactingWriter(target io.Writer) *redactingWriter {
	return newRedactingWriterWith(target, remoteRedactor{})
}

func newRedactingWriterWith(target io.Writer, redactor remoteRedactor) *redactingWriter {
	return &redactingWriter{target: target, redact: redactor.Redact}
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
		if _, err := io.WriteString(w.target, w.redact(line)); err != nil {
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
	_, err := io.WriteString(w.target, w.redact(string(w.pending)))
	w.pending = w.pending[:0]
	return err
}
