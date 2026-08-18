package omnigent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPinnedOmnigentLiveReplacementContinuity is an opt-in paid/network
// continuity proof. An external supervisor replaces the sidecar after the
// ready barrier appears, then creates the done barrier only after the
// replacement has bound a new socket against the same durable state.
func TestPinnedOmnigentLiveReplacementContinuity(t *testing.T) {
	socket := strings.TrimSpace(os.Getenv("GC_OMNIGENT_LIVE_SOCKET"))
	profileID := strings.TrimSpace(os.Getenv("GC_OMNIGENT_LIVE_PROFILE"))
	workspace := filepath.Clean(strings.TrimSpace(os.Getenv("GC_OMNIGENT_LIVE_WORKSPACE")))
	barrierDir := filepath.Clean(strings.TrimSpace(os.Getenv("GC_OMNIGENT_LIVE_REPLACE_DIR")))
	if socket == "" || profileID == "" || !filepath.IsAbs(workspace) || !filepath.IsAbs(barrierDir) {
		t.Skip("set GC_OMNIGENT_LIVE_SOCKET, GC_OMNIGENT_LIVE_PROFILE, absolute GC_OMNIGENT_LIVE_WORKSPACE, and absolute GC_OMNIGENT_LIVE_REPLACE_DIR")
	}

	readyPath := filepath.Join(barrierDir, "sidecar-replacement.ready")
	donePath := filepath.Join(barrierDir, "sidecar-replacement.done")
	for _, path := range []string{readyPath, donePath} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("replacement barrier already exists: %s", filepath.Base(path))
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect replacement barrier: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(path) })
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	client, err := NewUnixAPIClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.LocalStatus(ctx, "")
	if err != nil || !status.Ready || status.Mode != "local" {
		t.Fatalf("initial local sidecar status not ready: %v", err)
	}
	profile, err := client.ResolveProfile(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}

	marker := fmt.Sprintf("gc-replacement-%d", time.Now().UnixNano())
	input := AttachmentOpenInput{
		ProfileID: profile.ID,
		Workspace: workspace,
		Title:     "Gas City live replacement continuity",
		Identity: GasCityIdentity{
			SessionID: "live-replacement-" + profile.ID,
			Agent:     "worker",
			Rig:       "live-replacement",
			City:      "live-replacement",
		},
	}
	descriptor, err := client.ResolveAttachment(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	activeClient := client
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = activeClient.PostControl(stopCtx, descriptor.ConversationID, "stop_session")
	}()

	runReplacementLiveTurn(ctx, t, client, descriptor, input,
		"Remember this marker exactly: "+marker+". Reply only ACK.")

	beforeSocket, err := os.Lstat(socket)
	if err != nil {
		t.Fatalf("inspect original sidecar socket: %v", err)
	}
	if err := os.MkdirAll(barrierDir, 0o700); err != nil {
		t.Fatalf("create replacement barrier directory: %v", err)
	}
	ready, err := os.OpenFile(readyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create replacement ready barrier: %v", err)
	}
	if err := ready.Close(); err != nil {
		t.Fatalf("close replacement ready barrier: %v", err)
	}

	waitForReplacementBarrier(ctx, t, donePath)
	afterSocket, err := os.Lstat(socket)
	if err != nil {
		t.Fatalf("inspect replacement sidecar socket: %v", err)
	}
	if os.SameFile(beforeSocket, afterSocket) {
		t.Fatal("replacement acknowledged without replacing the sidecar socket")
	}

	client, err = NewUnixAPIClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	activeClient = client
	waitForLiveConversation(ctx, t, client, descriptor.ConversationID)
	input.ConversationID = descriptor.ConversationID
	resumed, err := client.ResolveAttachment(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Fresh || resumed.ConversationID != descriptor.ConversationID {
		t.Fatal("replacement did not resume the exact conversation")
	}

	runReplacementLiveTurn(ctx, t, client, resumed, input,
		"Reply only with the marker I asked you to remember before replacement.")
	snapshot, err := client.GetSession(ctx, descriptor.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if !assistantItemsContain(snapshot.Items, marker) {
		t.Fatal("replacement conversation did not recall the pre-replacement marker")
	}
}

func runReplacementLiveTurn(ctx context.Context, t *testing.T, client *APIClient, descriptor AttachmentDescriptor, input AttachmentOpenInput, prompt string) {
	t.Helper()
	attachment, err := client.OpenResolvedAttachment(ctx, descriptor, input)
	if err != nil {
		t.Fatal(err)
	}
	turnCtx, cancel := context.WithCancel(ctx)
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- attachment.Stream.Consume(turnCtx, func(StreamEvent) error { return nil })
	}()
	defer func() {
		cancel()
		_ = attachment.Close()
		select {
		case <-streamDone:
		case <-time.After(5 * time.Second):
			t.Error("live replacement stream did not stop after detach")
		}
	}()

	before, err := client.GetSession(ctx, descriptor.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	beforeAssistantItems := assistantItemCount(before.Items)
	queued, err := postLiveMessageWhenRunnerReady(ctx, client, descriptor.ConversationID, prompt)
	if err != nil || !queued {
		t.Fatalf("post live replacement message queued=%t: %v", queued, err)
	}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case streamErr := <-streamDone:
			t.Fatalf("live replacement stream ended before committed output: %v", streamErr)
		case <-ticker.C:
			snapshot, getErr := client.GetSession(ctx, descriptor.ConversationID)
			if getErr == nil && assistantItemCount(snapshot.Items) > beforeAssistantItems {
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func postLiveMessageWhenRunnerReady(ctx context.Context, client *APIClient, conversationID, prompt string) (bool, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		queued, err := client.PostMessage(ctx, conversationID, prompt)
		if err == nil || queued {
			return queued, err
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 || apiErr.Code != "runner_unavailable" {
			return false, err
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return false, fmt.Errorf("wait for live Omnigent runner: %w", err)
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func waitForReplacementBarrier(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := os.Lstat(path); err == nil {
				return
			} else if !os.IsNotExist(err) {
				t.Fatalf("inspect replacement done barrier: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func waitForLiveConversation(ctx context.Context, t *testing.T, client *APIClient, conversationID string) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := client.LocalStatus(ctx, conversationID)
		if err == nil && status.Ready && status.Conversation != nil && status.Conversation.ID == conversationID {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func assistantItemCount(items []SessionItem) int {
	count := 0
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Role), "assistant") {
			count++
		}
	}
	return count
}

func assistantItemsContain(items []SessionItem, want string) bool {
	for _, item := range items {
		if !strings.EqualFold(strings.TrimSpace(item.Role), "assistant") {
			continue
		}
		for _, block := range item.Content {
			if strings.Contains(block.Text, want) {
				return true
			}
		}
	}
	return false
}
