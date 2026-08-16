package ssh

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestSSHCapsuleStartUsesExactAllocatedStateAndCapsuleCommand(t *testing.T) {
	t.Parallel()
	cfg := testSSHCapsuleConfig(t)
	key := cfg.Capsule.Key
	cfg.Capsule.State = runtime.CapsuleStateReference{
		Key: key, Provider: string(runtime.SecretProviderSSH), ResourceID: key.ResourceStem(),
		ResourceUID: "7:42", MountPath: cfg.Capsule.State.MountPath,
	}
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case len(argv) >= 3 && argv[0] == "sh" && argv[2] == remoteOpenCapsuleStateScript:
			return []byte("7:42\n"), 0, nil
		case isTmux("has-session")(argv):
			return nil, 1, nil
		default:
			return successfulCapsulePreflightResponse(argv)
		}
	}}
	provider := providerWith(f)
	if err := provider.Start(context.Background(), "ga-ssh-capsule", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	statePath, runRoot, socketPath, catalogRoot := provider.capsulePhysicalPaths(cfg.Capsule)
	wantCommand := []string{
		"gc", "omnigent", "attach", "--mode", "capsule",
		"--socket", socketPath, "--state-root", statePath,
		"--catalog", filepath.Join(catalogRoot, "profiles.yaml"),
		"--profile", "claude-primary",
	}
	newSession := firstCall(f, isTmux("new-session"))
	if len(newSession) == 0 || newSession[len(newSession)-1] != shellquote.Join(wantCommand) {
		t.Fatalf("capsule new-session command = %#v, want %q", newSession, shellquote.Join(wantCommand))
	}
	if runRoot != filepath.Dir(socketPath) {
		t.Fatalf("run root/socket = %q, %q", runRoot, socketPath)
	}
	wantOption := []string{"tmux", "set-option", "-t", "ga-ssh-capsule", capsuleStateTMUXOption, "7:42"}
	if call := firstCall(f, isTmux("set-option")); !slices.Equal(call, wantOption) {
		t.Fatalf("state attachment option = %#v, want %#v", call, wantOption)
	}
}

func TestSSHCapsuleStartRejectsMissingOrStaleStateBeforeTmux(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  []byte
		code int
	}{
		{name: "missing", code: 44},
		{name: "stale uid", out: []byte("7:99\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testSSHCapsuleConfig(t)
			key := cfg.Capsule.Key
			cfg.Capsule.State = runtime.CapsuleStateReference{
				Key: key, Provider: string(runtime.SecretProviderSSH), ResourceID: key.ResourceStem(),
				ResourceUID: "7:42", MountPath: cfg.Capsule.State.MountPath,
			}
			f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
				if len(argv) >= 3 && argv[0] == "sh" && argv[2] == remoteOpenCapsuleStateScript {
					return tc.out, tc.code, nil
				}
				return successfulCapsulePreflightResponse(argv)
			}}
			err := providerWith(f).Start(context.Background(), "ga-ssh-capsule", cfg)
			if !errors.Is(err, runtime.ErrCapsuleStateConflict) {
				t.Fatalf("Start error = %v, want ErrCapsuleStateConflict", err)
			}
			if firstCall(f, isTmux("new-session")) != nil {
				t.Fatalf("invalid state launched tmux: %#v", f.calls)
			}
		})
	}
}

func TestSSHCapsuleStateAttachmentUsesLiveTmuxInventory(t *testing.T) {
	t.Parallel()
	key, _ := runtime.NewCapsuleKey("city", "session")
	ref := runtime.CapsuleStateReference{
		Key: key, Provider: string(runtime.SecretProviderSSH), ResourceID: key.ResourceStem(),
		ResourceUID: "7:42", MountPath: defaultCapsuleStateRoot,
	}
	attachments := "other\t7:42"
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case len(argv) >= 3 && argv[0] == "sh" && argv[2] == remoteOpenCapsuleStateScript:
			return []byte("7:42\n"), 0, nil
		case isTmux("list-sessions")(argv):
			return []byte(attachments), 0, nil
		default:
			return nil, 0, nil
		}
	}}
	p := providerWith(f)
	if err := p.AttachCapsuleState(context.Background(), "session", ref); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("cross-Place AttachCapsuleState error = %v, want ErrCapsuleStateConflict", err)
	}
	attachments = "session\t7:42"
	if err := p.AttachCapsuleState(context.Background(), "session", ref); err != nil {
		t.Fatalf("idempotent AttachCapsuleState: %v", err)
	}
	if err := p.DetachCapsuleState(context.Background(), "session"); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("DetachCapsuleState while tmux exists = %v, want conflict", err)
	}
	attachments = ""
	if err := p.DetachCapsuleState(context.Background(), "session"); err != nil {
		t.Fatalf("DetachCapsuleState after tmux loss: %v", err)
	}
}
