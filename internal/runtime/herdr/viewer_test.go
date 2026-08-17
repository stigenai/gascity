package herdr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
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

func TestViewerProjectionForwardsRawTerminalActionsExactlyOnce(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const target = "city--remote__worker"
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatal(err)
	}

	inputs := []string{
		"first line\nsecond line 世界🙂 $(literal); 'quoted'",
		"rapid-01",
		"rapid-02",
		"rapid-03",
	}
	for _, input := range inputs {
		if err := viewers.SendText(context.Background(), target, input); err != nil {
			t.Fatalf("SendText(%q): %v", input, err)
		}
	}
	if err := viewers.SendKeys(context.Background(), target, "enter", "ctrl+c"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if err := viewers.Resize(context.Background(), target, ViewerResize{Direction: "right", Amount: 0.125}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	output, err := viewers.Read(context.Background(), target, 200)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if output != "VIEWER_OUTPUT_世界🙂\n" {
		t.Fatalf("Read output = %q", output)
	}

	wantInput := strings.Join(inputs, "")
	gotInput, err := os.ReadFile(filepath.Join(state, "viewer-input"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotInput) != wantInput {
		t.Fatalf("forwarded input = %q, want exactly once %q", gotInput, wantInput)
	}
	calls := fakeCalls(t, state)
	if got := strings.Count(calls, "pane send-text %5"); got != len(inputs) {
		t.Fatalf("send-text calls = %d, want %d:\n%s", got, len(inputs), calls)
	}
	if !strings.Contains(calls, "pane send-keys %5 enter ctrl+c") {
		t.Fatalf("Enter/Ctrl-C not forwarded:\n%s", calls)
	}
	if !strings.Contains(calls, "pane resize --pane %5 --direction right --amount 0.125") {
		t.Fatalf("resize not forwarded:\n%s", calls)
	}
}

func TestViewerProjectionAttachPreservesRawStdioAndDetachIsNeutral(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const target = "city--remote__worker"
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatal(err)
	}

	rawInput := []byte("line one\n世界🙂\x03\x1b[8;41;119t")
	var rawOutput bytes.Buffer
	viewers.attachStdin = bytes.NewReader(rawInput)
	viewers.attachStdout = &rawOutput
	viewers.attachStderr = io.Discard
	var gotBin string
	var gotArgs []string
	viewers.runAttachCommand = func(_ context.Context, bin string, args []string, stdin io.Reader, stdout, _ io.Writer) error {
		gotBin = bin
		gotArgs = append([]string(nil), args...)
		_, err := io.Copy(stdout, stdin)
		return err
	}
	if err := viewers.Attach(context.Background(), target); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if gotBin != p.c.bin || strings.Join(gotArgs, " ") != "--session "+session+" %5" {
		t.Fatalf("Herdr attach command = %q %q", gotBin, gotArgs)
	}
	if !bytes.Equal(rawOutput.Bytes(), rawInput) {
		t.Fatalf("raw terminal bytes changed: got %q want %q", rawOutput.Bytes(), rawInput)
	}
	if _, ok, err := viewers.Binding(target); err != nil || !ok {
		t.Fatalf("detach removed viewer binding: ok=%v err=%v", ok, err)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "tab close") || strings.Contains(calls, "pane close") || strings.Contains(calls, "session stop") {
		t.Fatalf("terminal detach changed lifecycle:\n%s", calls)
	}
}

func TestViewerProjectionAllowsIndependentConcurrentTerminalViewers(t *testing.T) {
	p, session, _ := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const target = "city--remote__worker"
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	viewers.runAttachCommand = func(ctx context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- viewers.Attach(context.Background(), target) }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-waitCtx.Done():
			t.Fatal("concurrent viewer was serialized behind the first interactive attachment")
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Attach: %v", err)
		}
	}
}

func TestViewerProjectionSerializesResizeStormWithoutDroppingActions(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const target = "city--remote__worker"
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatal(err)
	}

	const actions = 12
	errs := make(chan error, actions)
	for range actions {
		go func() {
			errs <- viewers.Resize(context.Background(), target, ViewerResize{Direction: "down", Amount: 0.01})
		}()
	}
	for range actions {
		if err := <-errs; err != nil {
			t.Fatalf("Resize: %v", err)
		}
	}
	if got := strings.Count(fakeCalls(t, state), "pane resize --pane %5 --direction down --amount 0.01"); got != actions {
		t.Fatalf("resize actions = %d, want %d", got, actions)
	}
	if err := viewers.Resize(context.Background(), target, ViewerResize{Direction: "diagonal", Amount: 1}); err == nil {
		t.Fatal("invalid resize direction succeeded")
	}
	if err := viewers.Resize(context.Background(), target, ViewerResize{Direction: "right", Amount: 0}); err == nil {
		t.Fatal("zero resize amount succeeded")
	}
}

func TestViewerProjectionActionFailureSurfacesWithoutRetryOrCleanup(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const target = "city--remote__worker"
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatal(err)
	}
	setState(t, state, "viewer_input_lost_response")
	if err := viewers.SendText(context.Background(), target, "once-only"); err == nil || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("SendText lost-response error = %v, want runtime unavailable", err)
	}
	gotInput, err := os.ReadFile(filepath.Join(state, "viewer-input"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotInput) != "once-only" {
		t.Fatalf("lost-response input = %q, want one delivery without retry", gotInput)
	}
	if got := strings.Count(fakeCalls(t, state), "pane send-text %5 once-only"); got != 1 {
		t.Fatalf("lost-response send attempts = %d, want 1", got)
	}
	if _, ok, err := viewers.Binding(target); err != nil || !ok {
		t.Fatalf("action failure removed binding: ok=%v err=%v", ok, err)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "tab close") || strings.Contains(calls, "pane close") {
		t.Fatalf("action failure cleaned up worker/viewer:\n%s", calls)
	}

	sentinel := errors.New("viewer connection dropped")
	viewers.runAttachCommand = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
		return sentinel
	}
	if err := viewers.Attach(context.Background(), target); !errors.Is(err, sentinel) {
		t.Fatalf("Attach connection error = %v, want %v", err, sentinel)
	}
}

func TestViewerProjectionRejectsActionsAfterDetachOrPaneReplacement(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const target = "city--remote__worker"
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(state, "rawcmd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(state, "busy")); err != nil {
		t.Fatal(err)
	}
	if err := viewers.SendKeys(context.Background(), target, "ctrl+c"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("detached SendKeys error = %v, want session not found", err)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "pane send-keys") {
		t.Fatalf("input reached detached local shell:\n%s", calls)
	}

	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "rawcmd"), []byte("exec foreign-process"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := viewers.Read(context.Background(), target, 10); err == nil || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("replacement Read error = %v, want runtime unavailable", err)
	}
}

func TestViewerProjectionRejectsUnboundedOrMalformedActionsBeforeHerdr(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	viewers := newViewerProjection(p.c, filepath.Join(p.metaDir, "viewers"), p.c.cityRoot)
	const target = "city--remote__worker"
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target}); err != nil {
		t.Fatal(err)
	}
	before := fakeCalls(t, state)
	if err := viewers.SendText(context.Background(), target, "bad\x00input"); err == nil {
		t.Fatal("NUL text succeeded")
	}
	if err := viewers.SendText(context.Background(), target, strings.Repeat("x", maxViewerActionBytes+1)); err == nil {
		t.Fatal("oversized text succeeded")
	}
	if err := viewers.SendKeys(context.Background(), target); err == nil {
		t.Fatal("empty keys succeeded")
	}
	if err := viewers.SendKeys(context.Background(), target, make([]string, maxViewerKeys+1)...); err == nil {
		t.Fatal("oversized key batch succeeded")
	}
	if _, err := viewers.Read(context.Background(), target, maxViewerReadLines+1); err == nil {
		t.Fatal("oversized read succeeded")
	}
	if after := fakeCalls(t, state); after != before {
		t.Fatalf("invalid action reached Herdr:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
