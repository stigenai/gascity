package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestViewerProjectionReusesOneLifecycleNeutralPane(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)

	first, err := viewers.Open(context.Background(), ViewerSpec{
		Session:      "city--rig__worker",
		Label:        "Worker · Claude primary",
		ProfileBlurb: "Primary Claude profile using the Anthropic backend.",
	})
	if err != nil {
		t.Fatalf("Open first viewer: %v", err)
	}
	second, err := viewers.Open(context.Background(), ViewerSpec{
		Session:      "city--rig__worker",
		Label:        "Worker · Claude primary",
		ProfileBlurb: "Primary Claude profile using the Anthropic backend.",
	})
	if err != nil {
		t.Fatalf("Open existing viewer: %v", err)
	}
	if first.PaneID == "" || second.PaneID != first.PaneID || second.TabID != first.TabID {
		t.Fatalf("viewer bindings = first %#v second %#v, want one stable pane", first, second)
	}
	if second.Label != "Worker · Claude primary" || second.ProfileBlurb != "Primary Claude profile using the Anthropic backend." {
		t.Fatalf("viewer metadata = %#v", second)
	}
	calls := fakeCalls(t, state)
	if got := strings.Count(calls, "pane run "); got != 1 {
		t.Fatalf("pane launches = %d, want 1:\n%s", got, calls)
	}
	if !strings.Contains(calls, "gc session attach --no-resume -- city--rig__worker") {
		t.Fatalf("viewer did not use the lifecycle-neutral, shell-quoted attach command:\n%s", calls)
	}
	for _, forbidden := range []string{"agent start", "agent stop", "session start", "session stop", "health"} {
		if strings.Contains(calls, forbidden) {
			t.Fatalf("viewer mutated worker lifecycle with %q:\n%s", forbidden, calls)
		}
	}
	if got, err := p.GetMeta("city--rig__worker", metaBoundPane); err != nil || got != "" {
		t.Fatalf("worker pane metadata = %q, %v; viewer binding must be separate", got, err)
	}
}

func TestViewerProjectionConcurrentOpenCreatesOnePane(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const callers = 8
	results := make(chan ViewerBinding, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			binding, err := viewers.Open(context.Background(), ViewerSpec{Session: "city--worker"})
			results <- binding
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
	var paneID string
	for binding := range results {
		if paneID == "" {
			paneID = binding.PaneID
		}
		if binding.PaneID != paneID {
			t.Fatalf("pane = %q, want stable %q", binding.PaneID, paneID)
		}
	}
	if got := strings.Count(fakeCalls(t, state), "pane run "); got != 1 {
		t.Fatalf("concurrent pane launches = %d, want 1", got)
	}
}

func TestViewerProjectionReconnectsDetachedPaneAndSurvivesHerdrReplacement(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const target = "city--rig__worker"

	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.Remove(filepath.Join(state, "rawcmd")); err != nil {
		t.Fatalf("simulate detach: %v", err)
	}
	if err := os.Remove(filepath.Join(state, "busy")); err != nil {
		t.Fatalf("simulate detached shell: %v", err)
	}
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatalf("reconnect detached viewer: %v", err)
	}
	if got := strings.Count(fakeCalls(t, state), "pane run "); got != 2 {
		t.Fatalf("pane launches after detach = %d, want 2", got)
	}

	setState(t, state, "pane_gone")
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatalf("restore after Herdr pane replacement: %v", err)
	}
	if got := strings.Count(fakeCalls(t, state), "workspace create"); got != 2 {
		t.Fatalf("workspace creates after replacement = %d, want 2", got)
	}
}

func TestViewerProjectionRefusesForeignBusyPaneAndClosesOnlyItsViewer(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const target = "city--rig__worker"

	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, "rawcmd"), []byte("exec unrelated-command"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err == nil || !strings.Contains(err.Error(), "different command") {
		t.Fatalf("Open foreign pane error = %v, want safe conflict", err)
	}
	if err := viewers.Close(context.Background(), target); err == nil || !strings.Contains(err.Error(), "different command") {
		t.Fatalf("Close foreign pane error = %v, want safe conflict", err)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "tab close") || strings.Contains(calls, "pane close") {
		t.Fatalf("foreign pane was closed:\n%s", calls)
	}

	if err := os.Remove(filepath.Join(state, "rawcmd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(state, "busy")); err != nil {
		t.Fatal(err)
	}
	if err := viewers.Close(context.Background(), target); err != nil {
		t.Fatalf("Close detached viewer: %v", err)
	}
	if calls := fakeCalls(t, state); !strings.Contains(calls, "tab close t1") {
		t.Fatalf("viewer tab was not closed exactly:\n%s", calls)
	}
	if _, ok, err := viewers.Binding(target); err != nil || ok {
		t.Fatalf("Binding after close = ok %v err %v, want absent", ok, err)
	}
}

func TestViewerProjectionValidatesTargetAndRefreshesPublicMetadata(t *testing.T) {
	p, session, _ := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)

	if _, err := viewers.Open(context.Background(), ViewerSpec{}); err == nil {
		t.Fatal("Open empty target succeeded")
	}
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: "worker\x00other"}); err == nil {
		t.Fatal("Open NUL target succeeded")
	}
	raw, argv := viewerAttachCommand("worker; touch /tmp/not-run")
	if !strings.Contains(raw, "'worker; touch /tmp/not-run'") || argv[len(argv)-1] != "worker; touch /tmp/not-run" {
		t.Fatalf("shell-safe viewer command = %q argv=%q", raw, argv)
	}
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: "worker", Label: "old", ProfileBlurb: "old backend"}); err != nil {
		t.Fatal(err)
	}
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: "worker", Label: "new", ProfileBlurb: "compatible backend"}); err != nil {
		t.Fatal(err)
	}
	binding, ok, err := viewers.Binding("worker")
	if err != nil || !ok {
		t.Fatalf("Binding = %#v, %v, %v", binding, ok, err)
	}
	if binding.Label != "new" || binding.ProfileBlurb != "compatible backend" {
		t.Fatalf("refreshed metadata = %#v", binding)
	}
	if info, err := os.Stat(viewers.stateDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("viewer state directory mode = %v err=%v, want 0700", infoMode(info), err)
	}
	if info, err := os.Stat(viewers.bindingPath("worker")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("viewer binding mode = %v err=%v, want 0600", infoMode(info), err)
	}
}

func TestViewerTabLabelsRemainUniqueWhenDisplayLabelsMatch(t *testing.T) {
	first := viewerTabLabel(ViewerSpec{Session: "city--one", Label: "Claude primary"})
	second := viewerTabLabel(ViewerSpec{Session: "city--two", Label: "Claude primary"})
	if first == second || !strings.Contains(first, "Claude primary") || !strings.Contains(second, "Claude primary") {
		t.Fatalf("viewer tab labels = %q and %q, want readable unique labels", first, second)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
