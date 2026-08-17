package ssh

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSSHCapsuleProjectsSelectedProfileCredentialsThroughPrivateLauncher(t *testing.T) {
	t.Parallel()
	cfg := testSSHCapsuleConfig(t)
	cfg.SecretReferences = []runtime.SecretReference{
		{
			ID: "claude-primary-home", MountPath: "/run/gascity/omnigent/credentials/claude-primary",
			SSH: &runtime.SSHSecretPathReference{Path: "/srv/gascity/secrets/claude-primary"},
		},
		{
			ID: "claude-secondary-home", MountPath: "/run/gascity/omnigent/credentials/claude-secondary",
			SSH: &runtime.SSHSecretPathReference{Path: "/srv/gascity/secrets/claude-secondary"},
		},
		{
			ID: "primary-backend-token", Environment: "CLAUDE_PRIMARY_TOKEN",
			SSH: &runtime.SSHSecretPathReference{Path: "/srv/gascity/secrets/primary-backend-token"},
		},
	}
	const credentialSentinel = "credential-value-must-never-cross-ssh"
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isCapsuleCredentialStageCall(argv) {
			return []byte(credentialSentinel), 0, nil
		}
		return successfulCapsulePreflightResponse(argv)
	}}
	p := providerWith(f)
	if err := p.Start(context.Background(), "ga-profile-chain", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	checks := matchingCallIndexes(f.calls, isCapsuleCredentialCheckCall)
	if len(checks) != len(cfg.SecretReferences) {
		t.Fatalf("private credential checks = %d, want %d: %#v", len(checks), len(cfg.SecretReferences), f.calls)
	}
	stageIndexes := matchingCallIndexes(f.calls, isCapsuleCredentialStageCall)
	if len(stageIndexes) != 1 {
		t.Fatalf("credential launcher stages = %d, want 1: %#v", len(stageIndexes), f.calls)
	}
	launcher := string(f.stdins[stageIndexes[0]])
	for _, want := range []string{
		"/srv/gascity/secrets/claude-primary", "/srv/gascity/secrets/claude-secondary",
		"/srv/gascity/secrets/primary-backend-token", "CLAUDE_PRIMARY_TOKEN",
	} {
		if !strings.Contains(launcher, want) {
			t.Fatalf("private launcher omitted selected reference %q", want)
		}
	}
	if strings.Contains(launcher, credentialSentinel) {
		t.Fatal("private launcher contains credential value returned by remote command")
	}

	newSession := firstCall(f, isTmux("new-session"))
	if newSession == nil {
		t.Fatal("capsule did not launch tmux")
	}
	command := newSession[len(newSession)-1]
	for _, forbidden := range []string{
		credentialSentinel, "CLAUDE_PRIMARY_TOKEN", "/srv/gascity/secrets",
		"claude-primary-home", "claude-secondary-home", "primary-backend-token",
	} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("tmux command metadata contains %q: %s", forbidden, command)
		}
	}
	if !strings.Contains(command, capsuleCredentialLauncherName) {
		t.Fatalf("tmux command does not use private credential launcher: %s", command)
	}
	if stageIndexes[0] >= callIndex(f.calls, isTmux("new-session")) {
		t.Fatalf("credential launcher was not committed before tmux start: %#v", f.calls)
	}
}

func TestSSHCapsuleCredentialPreflightRejectsUnsafeSourcesWithoutDisclosure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		code int
	}{
		{name: "missing", code: 44},
		{name: "symlink", code: 73},
		{name: "wrong owner", code: 74},
		{name: "group or world readable", code: 75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testSSHCapsuleConfig(t)
			const credentialSentinel = "credential-value-must-not-appear-in-error"
			f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
				if isCapsuleCredentialCheckCall(argv) {
					return []byte(credentialSentinel), tc.code, nil
				}
				return successfulCapsulePreflightResponse(argv)
			}}
			err := providerWith(f).Start(context.Background(), "ga-private-source", cfg)
			var preflightErr *CapsulePreflightError
			if !errors.As(err, &preflightErr) || preflightErr.Kind != CapsulePreflightMissingProfileAuth {
				t.Fatalf("Start error = %v, want missing-profile-auth", err)
			}
			for _, forbidden := range []string{credentialSentinel, cfg.SecretReferences[0].SSH.Path} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("preflight error disclosed %q: %v", forbidden, err)
				}
			}
			if firstCall(f, isTmux("new-session")) != nil {
				t.Fatalf("unsafe credential source launched tmux: %#v", f.calls)
			}
		})
	}
}

func TestSSHCapsuleRejectsLiteralCredentialEnvironment(t *testing.T) {
	t.Parallel()
	cfg := testSSHCapsuleConfig(t)
	const sentinel = "literal-credential-must-not-leak"
	cfg.Env = map[string]string{"ANTHROPIC_API_KEY": sentinel}
	f := &fakeRunner{respond: successfulCapsulePreflightResponse}
	err := providerWith(f).Start(context.Background(), "ga-literal", cfg)
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("literal credential error = %v", err)
	}
	if firstCall(f, isTmux("new-session")) != nil {
		t.Fatal("literal credential environment launched tmux")
	}
}

func TestSSHCapsuleCredentialLauncherRecoversCommittedResponseLoss(t *testing.T) {
	t.Parallel()
	cfg := testSSHCapsuleConfig(t)
	stageAttempts := 0
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isCapsuleCredentialStageCall(argv):
			stageAttempts++
			return nil, -1, context.DeadlineExceeded
		case isCapsuleVerifyCall(argv):
			return nil, 0, nil
		default:
			return successfulCapsulePreflightResponse(argv)
		}
	}}
	if err := providerWith(f).Start(context.Background(), "ga-secret-reconnect", cfg); err != nil {
		t.Fatalf("Start after committed response loss: %v", err)
	}
	if stageAttempts != 1 || firstCall(f, isTmux("new-session")) == nil {
		t.Fatalf("credential response-loss recovery calls = %#v", f.calls)
	}
}

func TestSSHCapsuleCredentialLauncherStageFailureIsRedacted(t *testing.T) {
	t.Parallel()
	cfg := testSSHCapsuleConfig(t)
	const sentinel = "credential-value-returned-by-host"
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isCapsuleCredentialStageCall(argv):
			return []byte(sentinel), 76, nil
		case isCapsuleVerifyCall(argv):
			return nil, 1, nil
		default:
			return successfulCapsulePreflightResponse(argv)
		}
	}}
	err := providerWith(f).Start(context.Background(), "ga-secret-stage-failure", cfg)
	var stageErr *CapsuleStageError
	if !errors.As(err, &stageErr) || stageErr.Kind != CapsuleStageRemoteWrite {
		t.Fatalf("Start error = %v, want remote-write stage failure", err)
	}
	for _, forbidden := range []string{sentinel, cfg.SecretReferences[0].SSH.Path} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("credential staging error disclosed %q: %v", forbidden, err)
		}
	}
	if firstCall(f, isTmux("new-session")) != nil {
		t.Fatal("failed credential launcher staging started tmux")
	}
}

func isCapsuleCredentialCheckCall(argv []string) bool {
	return len(argv) == 6 && argv[0] == "sh" && argv[1] == "-c" && argv[3] == "gc-capsule-secret-check-v1"
}

func isCapsuleCredentialStageCall(argv []string) bool {
	return len(argv) == 8 && argv[0] == "sh" && argv[1] == "-c" && argv[3] == "gc-capsule-secret-stage-v1" &&
		slices.Contains(argv, capsuleCredentialLauncherName)
}
