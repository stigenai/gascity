package omnigent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPinnedOmnigentLiveProfiles is an opt-in paid/network smoke against an
// already supervised local sidecar. Fixtures contain no credentials; profiles
// resolve authentication exclusively through the operator's local catalog.
func TestPinnedOmnigentLiveProfiles(t *testing.T) {
	socket := strings.TrimSpace(os.Getenv("GC_OMNIGENT_LIVE_SOCKET"))
	profilesValue := strings.TrimSpace(os.Getenv("GC_OMNIGENT_LIVE_PROFILES"))
	workspace := filepath.Clean(strings.TrimSpace(os.Getenv("GC_OMNIGENT_LIVE_WORKSPACE")))
	if socket == "" || profilesValue == "" || !filepath.IsAbs(workspace) {
		t.Skip("set GC_OMNIGENT_LIVE_SOCKET, GC_OMNIGENT_LIVE_PROFILES, and absolute GC_OMNIGENT_LIVE_WORKSPACE for pinned Codex/Claude live smoke")
	}
	client, err := NewUnixAPIClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	status, err := client.LocalStatus(ctx, "")
	if err != nil || !status.Ready || status.Mode != "local" {
		t.Fatalf("local sidecar status = %#v, %v", status, err)
	}

	for index, rawProfile := range strings.Split(profilesValue, ",") {
		profileID := strings.TrimSpace(rawProfile)
		if profileID == "" {
			continue
		}
		t.Run(profileID, func(t *testing.T) {
			profile, err := client.ResolveProfile(ctx, profileID)
			if err != nil {
				t.Fatal(err)
			}
			sessionID := fmt.Sprintf("live-smoke-%d-%d", index, time.Now().UnixNano())
			input := AttachmentOpenInput{
				ProfileID: profile.ID, Workspace: workspace, Title: "Gas City pinned live smoke",
				Identity: GasCityIdentity{SessionID: sessionID, Agent: "worker", Rig: "live-smoke", City: "live-smoke"},
			}
			descriptor, err := client.ResolveAttachment(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer stopCancel()
				_ = client.PostControl(stopCtx, descriptor.ConversationID, "stop_session")
			}()
			attachment, err := client.OpenResolvedAttachment(ctx, descriptor, input)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := attachment.Close(); err != nil {
					t.Errorf("close attachment: %v", err)
				}
			}()
			output := make(chan struct{}, 1)
			streamDone := make(chan error, 1)
			go func() {
				streamDone <- attachment.Stream.Consume(ctx, func(event StreamEvent) error {
					if strings.TrimSpace(event.Delta) != "" || event.Item != nil {
						select {
						case output <- struct{}{}:
						default:
						}
					}
					return nil
				})
			}()
			if queued, err := client.PostMessage(ctx, descriptor.ConversationID, "Reply with one short sentence confirming the live harness is responsive."); err != nil || !queued {
				t.Fatalf("PostMessage queued=%t error=%v", queued, err)
			}
			select {
			case <-output:
			case err := <-streamDone:
				t.Fatalf("stream ended before output: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
