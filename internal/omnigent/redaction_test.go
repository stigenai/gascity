package omnigent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRedactingWriterHandlesSplitSecretsAndPartialFinalLine(t *testing.T) {
	const sentinel = "SENTINEL-SECRET-VALUE"
	var output bytes.Buffer
	writer := newRedactingWriter(&output)
	for _, fragment := range []string{
		"starting token=SENTINEL-", "SECRET-VALUE\nbackend https://user:", sentinel,
		"@model.example/v1\nfinal bearer ", sentinel,
	} {
		if _, err := writer.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), sentinel) || !strings.Contains(output.String(), "[redacted]") {
		t.Fatalf("redacted output = %q", output.String())
	}
}

func TestRemoteRedactorRemovesKnownValuesNamesAndPathsAcrossFragments(t *testing.T) {
	t.Parallel()
	const (
		secretValue    = "raw-value-remote-canary-8c437ea9"
		environment    = "CLAUDE_REMOTE_CANARY_AUTH"
		credentialPath = "/srv/remote-canary/auth/profile"
		statePath      = "/provider/private/remote-canary-state"
	)
	redactor := newRemoteRedactor(
		[]string{environment + "=" + secretValue},
		[]runtime.SecretReference{{
			ID: "profile-ref", Environment: environment,
			SSH: &runtime.SSHSecretPathReference{Path: credentialPath},
		}},
		statePath,
	)
	var output bytes.Buffer
	writer := newRedactingWriterWith(&output, redactor)
	for _, fragment := range []string{
		"raw ", secretValue[:10], secretValue[10:] + "\n",
		"environment " + environment + "\ncredential " + credentialPath[:12],
		credentialPath[12:] + "\nstate " + statePath,
	} {
		if _, err := writer.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secretValue, environment, credentialPath, statePath} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("redacted output leaked %q: %q", forbidden, output.String())
		}
	}
	if strings.Count(output.String(), "[redacted]") < 4 {
		t.Fatalf("redacted output = %q, want markers for every sensitive class", output.String())
	}
}

func TestRedactedClientErrorPreservesTypedClassificationWithoutRawBody(t *testing.T) {
	t.Parallel()
	const sentinel = "raw-backend-body-remote-canary-19c511"
	cause := &APIError{StatusCode: 503, Code: "backend_unavailable", Message: sentinel}
	err := redactedClientError("probe remote capsule", cause)
	var got *APIError
	if !errors.As(err, &got) || got != cause {
		t.Fatalf("redacted error lost typed cause: %T %v", err, err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("redacted error leaked backend body: %v", err)
	}
	if !strings.Contains(err.Error(), "backend_unavailable") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("redacted error lost actionable category: %v", err)
	}

	transportCause := fmt.Errorf("dial private path %s: %w", sentinel, context.DeadlineExceeded)
	transportErr := redactedClientError("probe remote capsule", transportCause)
	if !errors.Is(transportErr, context.DeadlineExceeded) {
		t.Fatalf("redacted transport error lost typed cause: %v", transportErr)
	}
	if strings.Contains(transportErr.Error(), sentinel) {
		t.Fatalf("redacted transport error leaked raw detail: %v", transportErr)
	}
}
