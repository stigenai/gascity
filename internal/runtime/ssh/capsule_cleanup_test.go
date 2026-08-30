package ssh

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSSHCapsuleStopTerminatesOnlyFencedProcessGroupAndRemovesEphemeralPaths(t *testing.T) {
	t.Parallel()
	key, err := runtime.NewCapsuleKey("ssh/box/city", "session-a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := runtime.NewCapsuleKey("ssh/box/city", "session-b")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("show-options")(argv):
			return []byte("7:42\n"), 0, nil
		case isCapsuleStateListCall(argv):
			return []byte(capsuleInventoryLine(key, "7:42") + capsuleInventoryLine(other, "8:43")), 0, nil
		default:
			return nil, 0, nil
		}
	}}
	p := providerWith(f)
	if err := p.Stop("session-a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	call := firstCall(f, isCapsuleCleanupCall)
	if call == nil {
		t.Fatalf("capsule cleanup call missing: %#v", f.calls)
	}
	joined := strings.Join(call, "\x00")
	for _, want := range []string{"session-a", key.ResourceStem(), "7:42", defaultCapsuleStateRoot, defaultCapsuleRunRoot, defaultCapsuleCatalogRoot} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cleanup call omitted exact identity %q: %#v", want, call)
		}
	}
	for _, forbidden := range []string{"session-b", other.ResourceStem(), "8:43", "kill-server"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("cleanup call touched unrelated identity %q: %#v", forbidden, call)
		}
	}
	script := call[2]
	for _, required := range []string{
		`pane_pid=$(tmux display-message`, `pane_pgid=$(ps`, `--state-root`,
		`tmux kill-session -t "$name"`, `kill -TERM "-$pane_pgid"`, `kill -KILL "-$pane_pgid"`,
		`run_path=$run_root/$resource`, `catalog_path=$catalog_root/$resource`, `rm -rf -- "$target"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("cleanup script missing %q", required)
		}
	}
	if strings.Contains(script, `rm -rf -- "$state_path"`) || strings.Contains(script, "tmux kill-server") {
		t.Fatalf("cleanup script can remove durable state or whole tmux server: %s", script)
	}
	if firstCall(f, isTmux("kill-session")) != nil {
		t.Fatal("capsule stop issued an unfenced standalone kill-session")
	}
}

func TestSSHCapsuleStopRecoversAfterTmuxLossAndRetainsState(t *testing.T) {
	t.Parallel()
	key, _ := runtime.NewCapsuleKey("city", "session-lost-tmux")
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("show-options")(argv):
			return nil, 1, nil
		case isTmux("has-session")(argv):
			return nil, 1, nil
		case slices.Equal(argv, []string{"test", "-d", defaultCapsuleStateRoot}):
			return nil, 0, nil
		case isCapsuleStateListCall(argv):
			return []byte(capsuleInventoryLine(key, "7:42")), 0, nil
		default:
			return nil, 0, nil
		}
	}}
	p := providerWith(f)
	if err := p.Stop("session-lost-tmux"); err != nil {
		t.Fatalf("Stop after tmux loss: %v", err)
	}
	if call := firstCall(f, isCapsuleCleanupCall); call == nil || !slices.Contains(call, key.ResourceStem()) {
		t.Fatalf("orphan cleanup call = %#v", call)
	}
	if firstCall(f, func(argv []string) bool {
		return len(argv) > 2 && argv[0] == "sh" && strings.Contains(argv[2], remotePurgeCapsuleStateScript)
	}) != nil {
		t.Fatal("Stop purged durable conversation state")
	}
}

func TestSSHCapsuleStopRetriesIdempotentlyAfterLostCleanupResponse(t *testing.T) {
	t.Parallel()
	key, _ := runtime.NewCapsuleKey("city", "session-response-loss")
	cleanupAttempts := 0
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("show-options")(argv):
			return []byte("7:42\n"), 0, nil
		case isCapsuleStateListCall(argv):
			return []byte(capsuleInventoryLine(key, "7:42")), 0, nil
		case isCapsuleCleanupCall(argv):
			cleanupAttempts++
			if cleanupAttempts == 1 {
				return nil, -1, context.DeadlineExceeded
			}
			return nil, 0, nil
		default:
			return nil, 0, nil
		}
	}}
	if err := providerWith(f).Stop("session-response-loss"); err != nil {
		t.Fatalf("Stop after committed response loss: %v", err)
	}
	if cleanupAttempts != 2 {
		t.Fatalf("cleanup attempts = %d, want one bounded verification retry", cleanupAttempts)
	}
}

func TestSSHStopMissingOrdinarySessionDoesNotRequireCapsuleStateRoot(t *testing.T) {
	t.Parallel()
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		switch {
		case isTmux("show-options")(argv):
			return nil, 1, nil
		case isTmux("has-session")(argv):
			return nil, 1, nil
		case slices.Equal(argv, []string{"test", "-d", defaultCapsuleStateRoot}):
			return nil, 1, nil
		case isTmux("kill-session")(argv):
			return nil, 1, nil
		default:
			return nil, 0, errors.New("unexpected remote call")
		}
	}}
	if err := providerWith(f).Stop("missing"); err != nil {
		t.Fatalf("Stop(missing ordinary session): %v", err)
	}
	if firstCall(f, isCapsuleCleanupCall) != nil {
		t.Fatal("ordinary missing session ran capsule cleanup")
	}
}

func capsuleInventoryLine(key runtime.CapsuleKey, uid string) string {
	return fmt.Sprintf("%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
		key.ResourceStem(), uid, key.Version, key.Digest, key.Token,
		hex.EncodeToString([]byte(key.CityScope)), hex.EncodeToString([]byte(key.SessionID)))
}

func isCapsuleStateListCall(argv []string) bool {
	return len(argv) >= 3 && argv[0] == "sh" && argv[2] == remoteListCapsuleStateScript
}

func isCapsuleCleanupCall(argv []string) bool {
	return len(argv) >= 4 && argv[0] == "sh" && argv[1] == "-c" && argv[3] == "gc-ssh-capsule-stop-v1"
}
