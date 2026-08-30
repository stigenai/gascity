package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/omnigent/inputframe"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestOmnigentRawPaneUsesStableBindingForLiveAttachAndInput(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	const name = "BrightLights--worker"
	const command = "gc omnigent attach --mode controller --profile claude-primary"
	workdir := t.TempDir()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: command,
		WorkDir: workdir,
		Env:     map[string]string{"GC_PROVIDER": inputframe.ControllerProvider},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Attach(name); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := p.Attach(name); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	if err := p.Interrupt(name); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	prompt := "first line\nsecond $(literal); 'quoted' 世界"
	if err := p.Nudge(name, runtime.TextContent(prompt)); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane run %5 exec /bin/sh -c") || !strings.Contains(calls, command) {
		t.Fatalf("Omnigent command did not launch in the placed pane:\n%s", calls)
	}
	if !strings.Contains(calls, "--cwd "+workdir) {
		t.Fatalf("Omnigent pane was not placed in its assigned workspace:\n%s", calls)
	}
	if strings.Contains(calls, "agent start") || strings.Contains(calls, "tmux") {
		t.Fatalf("Omnigent pane used a native agent kind or nested tmux:\n%s", calls)
	}
	if strings.Count(calls, "agent attach %5") != 2 || strings.Contains(calls, "agent attach brightlights--worker") {
		t.Fatalf("live attach did not target the stable bound pane:\n%s", calls)
	}
	if !strings.Contains(calls, "pane send-keys %5 Enter") || !strings.Contains(calls, "pane send-keys %5 ctrl+c") {
		t.Fatalf("input/interrupt did not target bound pane:\n%s", calls)
	}
	gotPrompt, err := os.ReadFile(filepath.Join(state, "rawcmd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotPrompt) != inputframe.Encode(prompt) {
		t.Fatalf("prompt = %q, want one framed message", gotPrompt)
	}
}
