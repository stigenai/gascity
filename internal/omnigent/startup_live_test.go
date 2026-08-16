package omnigent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPinnedOmnigentLocalStartup is an opt-in credential-free smoke of the
// reviewed executable, real agent registration, local proxy, and durable
// conversation resume. The paid/network profile matrix remains separate.
func TestPinnedOmnigentLocalStartup(t *testing.T) {
	executable := strings.TrimSpace(os.Getenv("GC_OMNIGENT_PINNED_EXECUTABLE"))
	commit := strings.TrimSpace(os.Getenv("GC_OMNIGENT_PINNED_COMMIT"))
	version := strings.TrimSpace(os.Getenv("GC_OMNIGENT_PINNED_VERSION"))
	if !filepath.IsAbs(executable) || commit == "" || version == "" {
		t.Skip("set absolute GC_OMNIGENT_PINNED_EXECUTABLE, GC_OMNIGENT_PINNED_COMMIT, and GC_OMNIGENT_PINNED_VERSION for credential-free pinned startup smoke")
	}
	digest, err := fileSHA256(executable)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	agentDir := filepath.Join(stateRoot, "config", "agents")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "offline.yaml"), []byte(`name: gascity-offline-smoke
description: Credential-free pinned startup fixture.
executor:
  harness: codex
  model: openai/gascity-offline-smoke
prompt: |
  Report only the assigned work.
`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(stateRoot, "config", "profiles.yaml")
	catalog := fmt.Sprintf(`version: 1
omnigent:
  commit: %q
  package_version: %q
  executable: %q
  sha256: %q
profiles:
  offline-smoke:
    display_name: Offline smoke
    blurb: Credential-free pinned local startup fixture.
    harness: codex
    backend: gascity-offline-smoke
    network: offline
    agent: agents/offline.yaml
`, commit, version, executable, "sha256:"+digest)
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/private/tmp", "gco")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "o.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- ServeSidecar(ctx, SidecarConfig{
			StateRoot: stateRoot, CatalogPath: catalogPath, SocketPath: socketPath,
			StartupTimeout: 30 * time.Second, ShutdownTimeout: 5 * time.Second,
			Stdout: &output, Stderr: &output,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Errorf("timed out cleaning up pinned Omnigent sidecar")
		}
	})
	client, err := NewUnixAPIClient(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(45 * time.Second)
	var lastStatusErr error
	for {
		status, statusErr := client.LocalStatus(context.Background(), "")
		if statusErr == nil && status.Ready && status.Mode == "local" {
			break
		}
		lastStatusErr = statusErr
		select {
		case err := <-done:
			t.Fatalf("ServeSidecar exited before readiness: %v\n%s", err, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for pinned sidecar readiness: %v\n%s", lastStatusErr, output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	input := AttachmentOpenInput{
		ProfileID: "offline-smoke", Workspace: stateRoot, Title: "Pinned startup smoke",
		Identity: GasCityIdentity{SessionID: "pinned-startup", Agent: "worker", Rig: "smoke", City: "smoke"},
	}
	fresh, err := client.ResolveAttachment(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.ConversationID = fresh.ConversationID
	resumed, err := client.ResolveAttachment(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Fresh || resumed.Fresh || resumed.ConversationID != fresh.ConversationID {
		t.Fatalf("fresh = %#v, resumed = %#v", fresh, resumed)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeSidecar shutdown: %v\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for pinned sidecar shutdown")
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("sidecar socket remains after shutdown: %v", err)
	}
}

func fileSHA256(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read executable digest: %w", err)
	}
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum[:]), nil
}
