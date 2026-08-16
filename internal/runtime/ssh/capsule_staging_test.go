package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProviderCapsuleStagesUnicodeInputsAtomicallyBeforeLaunch(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "catalog source with spaces 多")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	agent := stagedTestInput(t, root, "agent ü.yaml", "agents/agent ü.yaml", []byte("name: unicode-agent\nprompt: work\n"))
	catalog := stagedTestInput(t, root, "profiles.yaml", "profiles.yaml", []byte("version: 1\nprofiles: {}\n"))
	credentialSentinel := "GC_CREDENTIAL_SENTINEL_must_not_transfer"
	if err := os.WriteFile(filepath.Join(root, "credential.txt"), []byte(credentialSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testSSHCapsuleConfig(t)
	cfg.Capsule.CatalogInputs = []runtime.CapsuleInput{agent, catalog}
	f := &fakeRunner{respond: successfulCapsulePreflightResponse}

	if err := providerWith(f).Start(context.Background(), "ga-stage", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stageCalls := matchingCallIndexes(f.calls, isCapsuleStageCall)
	if len(stageCalls) != 2 {
		t.Fatalf("stage calls = %d, want 2: %#v", len(stageCalls), f.calls)
	}
	for i, index := range stageCalls {
		want := []byte("name: unicode-agent\nprompt: work\n")
		if i == 1 {
			want = []byte("version: 1\nprofiles: {}\n")
		}
		if !slices.Equal(f.stdins[index], want) {
			t.Fatalf("stage stdin[%d] = %q, want %q", i, f.stdins[index], want)
		}
		if strings.Contains(string(f.stdins[index]), credentialSentinel) {
			t.Fatal("staging transferred adjacent credential sentinel")
		}
		if !strings.Contains(f.calls[index][2], ".gc-stage.$$") || !strings.Contains(f.calls[index][2], "mv -f") {
			t.Fatalf("stage script is not atomic: %s", f.calls[index][2])
		}
		if got := f.calls[index][len(f.calls[index])-1]; got != "0644" {
			t.Fatalf("stage mode = %q, want 0644", got)
		}
	}
	if stageCalls[1] >= callIndex(f.calls, isTmux("new-session")) {
		t.Fatalf("catalog staging did not finish before tmux launch: %#v", f.calls)
	}
}

func TestProviderCapsuleStagingFailureMatrixPreventsLaunch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		kind      CapsuleStageFailureKind
		prepare   func(*testing.T, *runtime.Config)
		stageCode int
	}{
		{
			name: "controller checksum mismatch", kind: CapsuleStageChecksumMismatch,
			prepare: func(t *testing.T, cfg *runtime.Config) {
				if err := os.WriteFile(cfg.Capsule.CatalogInputs[0].SourcePath, []byte("changed after plan"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink source", kind: CapsuleStageInvalidSource,
			prepare: func(t *testing.T, cfg *runtime.Config) {
				target := cfg.Capsule.CatalogInputs[0].SourcePath
				link := filepath.Join(t.TempDir(), "catalog-link")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				cfg.Capsule.CatalogInputs[0].SourcePath = link
			},
		},
		{name: "remote symlink escape", kind: CapsuleStageContainment, stageCode: 73},
		{name: "remote checksum mismatch", kind: CapsuleStageChecksumMismatch, stageCode: 74},
		{name: "partial remote write", kind: CapsuleStageRemoteWrite, stageCode: 76},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testSSHCapsuleConfig(t)
			if tc.prepare != nil {
				tc.prepare(t, &cfg)
			}
			f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
				switch {
				case tc.stageCode != 0 && isCapsuleStageCall(argv):
					return nil, tc.stageCode, nil
				case isCapsuleVerifyCall(argv):
					return nil, 1, nil
				default:
					return successfulCapsulePreflightResponse(argv)
				}
			}}
			err := providerWith(f).Start(context.Background(), "ga-stage", cfg)
			var stageErr *CapsuleStageError
			if !errors.As(err, &stageErr) || stageErr.Kind != tc.kind {
				t.Fatalf("Start error = %v, want stage kind %q", err, tc.kind)
			}
			if firstCall(f, isTmux("new-session")) != nil {
				t.Fatalf("failed staging launched tmux: %#v", f.calls)
			}
		})
	}
}

func TestProviderCapsuleStagingReconnectVerifiesCommittedReplacement(t *testing.T) {
	t.Parallel()
	cfg := testSSHCapsuleConfig(t)
	stageAttempts := 0
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isCapsuleStageCall(argv):
			stageAttempts++
			return nil, -1, context.DeadlineExceeded
		case isCapsuleVerifyCall(argv):
			return nil, 0, nil
		default:
			return successfulCapsulePreflightResponse(argv)
		}
	}}
	if err := providerWith(f).Start(context.Background(), "ga-stage", cfg); err != nil {
		t.Fatalf("Start after committed response loss: %v", err)
	}
	if stageAttempts != 1 || firstCall(f, isTmux("new-session")) == nil {
		t.Fatalf("staging reconnect calls = %#v", f.calls)
	}
}

func TestProviderCapsuleStagingIsIdempotentForSameGeneration(t *testing.T) {
	t.Parallel()
	cfg := testSSHCapsuleConfig(t)
	f := &fakeRunner{respond: successfulCapsulePreflightResponse}
	p := providerWith(f)
	if err := p.stageCapsuleInputs(context.Background(), cfg); err != nil {
		t.Fatalf("first stage: %v", err)
	}
	if err := p.stageCapsuleInputs(context.Background(), cfg); err != nil {
		t.Fatalf("second stage: %v", err)
	}
	indexes := matchingCallIndexes(f.calls, isCapsuleStageCall)
	if len(indexes) != 2 || !slices.Equal(f.calls[indexes[0]], f.calls[indexes[1]]) || !slices.Equal(f.stdins[indexes[0]], f.stdins[indexes[1]]) {
		t.Fatalf("repeated generation was not byte-identical: calls=%#v", f.calls)
	}
	if !strings.Contains(f.calls[indexes[0]][2], `if [ -f "$target" ]`) {
		t.Fatal("remote stage protocol lacks existing-generation fast path")
	}
}

func TestSSHPlaceStageUsesContainedAtomicCopyBoundary(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "copy source.txt")
	if err := os.WriteFile(source, []byte("copy bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{respond: successfulCapsulePreflightResponse}
	p := providerWith(f)
	p.workDirs = map[string]string{"session": "/srv/gascity/work"}
	place := &sshPlace{p: p, name: "session"}
	if err := place.Stage(context.Background(), []runtime.CopyEntry{{Src: source, RelDst: "nested/δ.txt"}}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	call := firstCall(f, isCapsuleStageCall)
	if call == nil || call[len(call)-4] != "/srv/gascity/work" || call[len(call)-3] != "nested/δ.txt" {
		t.Fatalf("Stage call = %#v", call)
	}
	if err := place.Stage(context.Background(), []runtime.CopyEntry{{Src: source, RelDst: "../escape"}}); err == nil {
		t.Fatal("Stage accepted destination escape")
	}
}

func stagedTestInput(t *testing.T, root, sourceName, relative string, payload []byte) runtime.CapsuleInput {
	t.Helper()
	path := filepath.Join(root, sourceName)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return runtime.CapsuleInput{
		SourcePath: path, RelativePath: relative,
		SHA256: "sha256:" + hex.EncodeToString(digest[:]), Mode: 0o644,
	}
}

func isCapsuleStageCall(argv []string) bool {
	return len(argv) == 8 && argv[0] == "sh" && argv[1] == "-c" && argv[3] == "gc-capsule-stage-v1"
}

func isCapsuleVerifyCall(argv []string) bool {
	return len(argv) == 8 && argv[0] == "sh" && argv[1] == "-c" && argv[3] == "gc-capsule-verify-v1"
}

func matchingCallIndexes(calls [][]string, predicate func([]string) bool) []int {
	var indexes []int
	for i, call := range calls {
		if predicate(call) {
			indexes = append(indexes, i)
		}
	}
	return indexes
}
