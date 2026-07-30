package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	corev1 "k8s.io/api/core/v1"
)

func TestTarDirStripsOwnership(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := tarDir(dir, &buf); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 {
			t.Errorf("entry %q: want UID/GID 0/0, got %d/%d", hdr.Name, hdr.Uid, hdr.Gid)
		}
		if hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("entry %q: want empty Uname/Gname, got %q/%q", hdr.Name, hdr.Uname, hdr.Gname)
		}
	}
}

func TestTarDirExcludesReproducibleCaches(t *testing.T) {
	dir := t.TempDir()
	included := map[string]string{
		"source.go":               "source",
		".claude/settings.json":   "settings",
		"ui/package.json":         "package",
		"nested/.next-source.txt": "not a cache directory",
	}
	excluded := map[string]string{
		".devenv/state/profile":          "nix cache",
		".direnv/cache":                  "direnv cache",
		"ui/.next/cache/bundle":          "next cache",
		"ui/node_modules/pkg/index.js":   "node cache",
		".claude/worktrees/task/file.go": "nested checkout",
	}
	for name, contents := range included {
		writeTarTestFile(t, dir, name, contents)
	}
	for name, contents := range excluded {
		writeTarTestFile(t, dir, name, contents)
	}

	var buf bytes.Buffer
	if err := tarDir(dir, &buf); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		got[filepath.ToSlash(hdr.Name)] = string(data)
	}
	for name, want := range included {
		if got[name] != want {
			t.Errorf("included file %q = %q, want %q", name, got[name], want)
		}
	}
	for name := range excluded {
		if _, ok := got[name]; ok {
			t.Errorf("cache file %q was included in archive", name)
		}
	}
}

func writeTarTestFile(t *testing.T, root, name, contents string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTarFileStripsOwnership(t *testing.T) {
	f := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := tarFile(f, info, "test.txt", &buf); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Uid != 0 || hdr.Gid != 0 {
		t.Errorf("want UID/GID 0/0, got %d/%d", hdr.Uid, hdr.Gid)
	}
	if hdr.Uname != "" || hdr.Gname != "" {
		t.Errorf("want empty Uname/Gname, got %q/%q", hdr.Uname, hdr.Gname)
	}
}

func TestStageFilesStagesKiroPackOverlayAtWorkspaceRoot(t *testing.T) {
	workDir := t.TempDir()
	projectInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(projectInstructions, []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", projectInstructions, err)
	}

	packOverlay := t.TempDir()
	agentConfig := filepath.Join(packOverlay, "per-provider", "kiro", ".kiro", "agents", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(agentConfig), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(agentConfig), err)
	}
	if err := os.WriteFile(agentConfig, []byte(`{"name":"gascity"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", agentConfig, err)
	}
	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", fallbackInstructions, err)
	}

	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, "", io.Discard)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}

	if got := ops.files["/workspace/.kiro/agents/gascity.json"]; got != `{"name":"gascity"}` {
		t.Fatalf("staged Kiro agent config = %q, want root gascity config", got)
	}
	if _, ok := ops.files["/workspace/per-provider/kiro/.kiro/agents/gascity.json"]; ok {
		t.Fatal("Kiro provider overlay should be flattened, not staged under per-provider/kiro")
	}
	if got := ops.files["/workspace/AGENTS.md"]; got != "project instructions" {
		t.Fatalf("staged AGENTS.md = %q, want project instructions preserved", got)
	}
}

func TestStageFilesStagesKiroPackOverlayAtPodWorkDirForRigWorkDir(t *testing.T) {
	cityRoot := t.TempDir()
	workDir := filepath.Join(cityRoot, "rigs", "team")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", workDir, err)
	}
	rigInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(rigInstructions, []byte("rig instructions"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", rigInstructions, err)
	}
	rigFile := filepath.Join(workDir, "task.txt")
	if err := os.WriteFile(rigFile, []byte("rig payload"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", rigFile, err)
	}

	packOverlay := t.TempDir()
	agentConfig := filepath.Join(packOverlay, "per-provider", "kiro", ".kiro", "agents", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(agentConfig), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(agentConfig), err)
	}
	if err := os.WriteFile(agentConfig, []byte(`{"name":"gascity"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", agentConfig, err)
	}
	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", fallbackInstructions, err)
	}

	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, cityRoot, io.Discard)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}

	if got := ops.files["/workspace/rigs/team/.kiro/agents/gascity.json"]; got != `{"name":"gascity"}` {
		t.Fatalf("staged Kiro agent config = %q, want rig workdir gascity config", got)
	}
	if _, ok := ops.files["/workspace/.kiro/agents/gascity.json"]; ok {
		t.Fatal("rig-mode Kiro agent config should be staged under pod workdir, not workspace root")
	}
	if _, ok := ops.files["/workspace/per-provider/kiro/.kiro/agents/gascity.json"]; ok {
		t.Fatal("Kiro provider overlay should be flattened, not staged under per-provider/kiro")
	}
	if got := ops.files["/workspace/rigs/team/AGENTS.md"]; got != "rig instructions" {
		t.Fatalf("staged rig AGENTS.md = %q, want rig instructions preserved", got)
	}
	if got := ops.files["/workspace/rigs/team/task.txt"]; got != "rig payload" {
		t.Fatalf("staged rig workdir payload = %q, want copied under rig-relative workspace path", got)
	}
}

func TestStageFilesKeepsExternalRigOutsideCityWorkspace(t *testing.T) {
	cityRoot := t.TempDir()
	workDir := t.TempDir()
	writeTarTestFile(t, workDir, ".beads/metadata.json", `{"dolt_database":"ib"}`)
	writeTarTestFile(t, workDir, "task.txt", "rig payload")

	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-external-rig", runtime.Config{
		WorkDir: workDir,
		Env:     map[string]string{"GC_CITY": cityRoot},
	}, cityRoot, io.Discard)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}

	if got := ops.files["/workspace/rig/.beads/metadata.json"]; got != `{"dolt_database":"ib"}` {
		t.Fatalf("external rig metadata = %q, want staged under /workspace/rig", got)
	}
	if got := ops.files["/workspace/rig/task.txt"]; got != "rig payload" {
		t.Fatalf("external rig payload = %q, want staged under /workspace/rig", got)
	}
	if _, ok := ops.files["/workspace/.beads/metadata.json"]; ok {
		t.Fatal("external rig metadata was staged at city root and can be overwritten by city initialization")
	}
}

func TestStageFilesProjectsExternalRigHookCopyIntoPodWorkDir(t *testing.T) {
	cityRoot := t.TempDir()
	workDir := t.TempDir()
	hookPath := filepath.Join(workDir, ".pi", "extensions", "gc-hooks.js")
	writeTarTestFile(t, workDir, ".pi/extensions/gc-hooks.js", "export default {}")

	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-external-rig", runtime.Config{
		WorkDir: workDir,
		Env:     map[string]string{"GC_CITY": cityRoot},
		CopyFiles: []runtime.CopyEntry{{
			Src: hookPath,
			RelDst: path.Join(
				filepath.ToSlash(mustRelativePath(t, cityRoot, workDir)),
				".pi", "extensions", "gc-hooks.js",
			),
		}},
	}, cityRoot, io.Discard)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}

	if got := ops.files["/workspace/rig/.pi/extensions/gc-hooks.js"]; got != "export default {}" {
		t.Fatalf("external rig hook = %q, want staged under projected pod workdir", got)
	}
	wantCopyDestination := "/workspace/rig/.pi/extensions"
	foundCopyDestination := false
	for _, destination := range ops.archiveDestinations {
		if destination == wantCopyDestination {
			foundCopyDestination = true
		}
		if destination != "/workspace" &&
			destination != "/workspace/rig" &&
			!strings.HasPrefix(destination, "/workspace/") {
			t.Fatalf("external rig hook escaped the workspace volume: %s", destination)
		}
	}
	if !foundCopyDestination {
		t.Fatalf("copy_file destinations = %v, want %s", ops.archiveDestinations, wantCopyDestination)
	}
}

func mustRelativePath(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q): %v", base, target, err)
	}
	return rel
}

func TestStageFilesUsesConcreteProviderOverlayName(t *testing.T) {
	workDir := t.TempDir()
	packOverlay := t.TempDir()

	kiroConfig := filepath.Join(packOverlay, "per-provider", "kiro", ".kiro", "agents", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(kiroConfig), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(kiroConfig), err)
	}
	if err := os.WriteFile(kiroConfig, []byte(`{"name":"gascity"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", kiroConfig, err)
	}
	claudeInstructions := filepath.Join(packOverlay, "per-provider", "claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claudeInstructions), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(claudeInstructions), err)
	}
	if err := os.WriteFile(claudeInstructions, []byte("claude instructions"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", claudeInstructions, err)
	}

	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:             workDir,
		ProviderName:        "claude",
		ProviderOverlayName: "kiro",
		PackOverlayDirs:     []string{packOverlay},
	}, "", io.Discard)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}

	if got := ops.files["/workspace/.kiro/agents/gascity.json"]; got != `{"name":"gascity"}` {
		t.Fatalf("staged Kiro agent config = %q, want root gascity config", got)
	}
	if _, ok := ops.files["/workspace/CLAUDE.md"]; ok {
		t.Fatal("staged Claude overlay for Kiro provider inheriting Claude launch behavior")
	}
}

func TestStageFilesSurfacesKiroPreservationWarning(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	packOverlay := t.TempDir()
	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(fallbackInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro fallback instructions: %v", err)
	}
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro fallback instructions: %v", err)
	}

	var warnings bytes.Buffer
	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, "", &warnings)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}
	if got := ops.files["/workspace/AGENTS.md"]; got != "project instructions" {
		t.Fatalf("staged AGENTS.md = %q, want project instructions preserved", got)
	}
	if got := warnings.String(); !strings.Contains(got, "overlay: preserving existing") || !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("warnings = %q, want Kiro preservation warning", got)
	}
}

func TestStageFilesPropagatesFatalProviderOverlayError(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	packOverlay := t.TempDir()
	nestedInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md", "nested.md")
	if err := os.MkdirAll(filepath.Dir(nestedInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro nested instructions: %v", err)
	}
	if err := os.WriteFile(nestedInstructions, []byte("nested instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro nested instructions: %v", err)
	}

	var warnings bytes.Buffer
	ops := newCapturingStageOps()
	err := stageFiles(context.Background(), ops, "gc-kiro", runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, "", &warnings)
	if err == nil {
		t.Fatal("stageFiles succeeded, want fatal provider overlay error")
	}
	if got := err.Error(); !strings.Contains(got, "staging pack overlay") || !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("stageFiles error = %q, want pack overlay AGENTS.md context", got)
	}
	if strings.Contains(warnings.String(), "staging pack overlay") {
		t.Fatalf("fatal provider overlay error was demoted to warning: %q", warnings.String())
	}
}

func TestWaitForExecReadySucceedsImmediately(t *testing.T) {
	ops := &execReadyOps{}

	if err := waitForExecReady(context.Background(), ops, "pod", time.Second); err != nil {
		t.Fatalf("waitForExecReady: %v", err)
	}
	if got := ops.calls; got != 1 {
		t.Fatalf("exec calls = %d, want 1", got)
	}
	if got := ops.commands[0]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("probe command = %v, want [true]", got)
	}
}

func TestWaitForExecReadyRetriesTransientErrors(t *testing.T) {
	ops := &execReadyOps{
		errors: []error{
			errors.New("container not found"),
			errors.New("container not found"),
			nil,
		},
	}

	if err := waitForExecReady(context.Background(), ops, "pod", 2*time.Second); err != nil {
		t.Fatalf("waitForExecReady: %v", err)
	}
	if got := ops.calls; got != 3 {
		t.Fatalf("exec calls = %d, want 3", got)
	}
}

func TestWaitForExecReadyTimeoutPreservesLastError(t *testing.T) {
	ops := &execReadyOps{errors: []error{errors.New("spdy endpoint unavailable")}}

	err := waitForExecReady(context.Background(), ops, "pod", time.Millisecond)
	if err == nil {
		t.Fatal("waitForExecReady succeeded, want timeout error")
	}
	if !strings.Contains(err.Error(), "exec not ready in pod/stage after 1ms") {
		t.Fatalf("error = %q, want timeout context", err)
	}
	if !errors.Is(err, ops.errors[0]) {
		t.Fatalf("error = %v, want wrapped last exec error %v", err, ops.errors[0])
	}
}

func TestWaitForExecReadyReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ops := &execReadyOps{errors: []error{errors.New("container not found")}}
	cancel()

	err := waitForExecReady(ctx, ops, "pod", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForExecReady error = %v, want context.Canceled", err)
	}
	if got := ops.calls; got != 0 {
		t.Fatalf("exec calls after context cancellation = %d, want 0", got)
	}
}

func TestWaitForExecReadyReturnsContextCancellationDuringDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	firstProbe := make(chan struct{})
	ops := &execReadyOps{
		errors: []error{errors.New("container not found")},
		afterExec: func() {
			go func() {
				time.Sleep(10 * time.Millisecond)
				cancel()
			}()
		},
		firstProbeCh: firstProbe,
	}

	err := waitForExecReady(ctx, ops, "pod", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForExecReady error = %v, want context.Canceled", err)
	}
	select {
	case <-firstProbe:
	default:
		t.Fatal("exec probe was not attempted before cancellation")
	}
	if got := ops.calls; got != 1 {
		t.Fatalf("exec calls = %d, want 1", got)
	}
}

func TestCopyDirToPodStreamsArchive(t *testing.T) {
	dir := t.TempDir()
	payload := strings.Repeat("bounded-stream-", 4096)
	if err := os.WriteFile(filepath.Join(dir, "payload.txt"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	ops := newCapturingStageOps()
	if err := copyDirToPod(context.Background(), ops, "pod", "stage", dir, "/workspace"); err != nil {
		t.Fatalf("copyDirToPod: %v", err)
	}
	if !ops.sawPipeReader {
		t.Fatal("copyDirToPod stdin was not streamed through io.Pipe")
	}
	if got := ops.files["/workspace/payload.txt"]; got != payload {
		t.Fatalf("staged payload length = %d, want %d", len(got), len(payload))
	}
}

func TestCopyDirToPodDereferencesDirectorySymlinks(t *testing.T) {
	dir := t.TempDir()
	releaseDir := filepath.Join(dir, ".managed", "packs", "release")
	if err := os.MkdirAll(filepath.Join(releaseDir, "rig-basic"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(releaseDir, "rig-basic", "pack.toml"),
		[]byte("name = \"rig-basic\"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(".managed", "packs", "release"), filepath.Join(dir, "packs")); err != nil {
		t.Fatal(err)
	}

	ops := newCapturingStageOps()
	if err := copyDirToPod(context.Background(), ops, "pod", "agent", dir, "/tmp/city-src"); err != nil {
		t.Fatalf("copyDirToPod: %v", err)
	}
	if got := ops.files["/tmp/city-src/packs/rig-basic/pack.toml"]; got != "name = \"rig-basic\"\n" {
		t.Fatalf("staged managed pack = %q, want authored pack contents", got)
	}
}

func TestInitCityInPodExcludesMutableRuntimeState(t *testing.T) {
	city := t.TempDir()
	included := map[string]string{
		"city.toml": "[workspace]\nname = \"factory\"\n",
		"pack.toml": "schema = 2\n",
		".managed/packs/release/rig-basic/pack.toml": "name = \"rig-basic\"\n",
		"agents/demo/state/schema.json":              "{}\n",
		"scripts/health.py":                          "print('ok')\n",
	}
	excluded := map[string]string{
		".gc/runtime/session/pane.log": "runtime",
		".beads/dolt/noms":             "database",
		".dolt-backup/city/manifest":   "backup",
		"state/route-claim-watch.json": "state",
		"logs/order.log":               "logs",
		"scratch-dolt/LOCK":            "scratch",
	}
	for name, contents := range included {
		writeTarTestFile(t, city, name, contents)
	}
	for name, contents := range excluded {
		writeTarTestFile(t, city, name, contents)
	}
	if err := os.Symlink(
		filepath.Join(".managed", "packs", "release"),
		filepath.Join(city, "packs"),
	); err != nil {
		t.Fatal(err)
	}

	ops := newCapturingStageOps()
	if err := initCityInPod(context.Background(), ops, "pod", city); err != nil {
		t.Fatalf("initCityInPod: %v", err)
	}
	for name, want := range included {
		got := ops.files[path.Join("/tmp/city-src", name)]
		if got != want {
			t.Errorf("authored city file %q = %q, want %q", name, got, want)
		}
	}
	if got := ops.files["/tmp/city-src/packs/rig-basic/pack.toml"]; got != "name = \"rig-basic\"\n" {
		t.Errorf("managed pack symlink contents = %q, want authored pack", got)
	}
	for name := range excluded {
		if _, ok := ops.files[path.Join("/tmp/city-src", name)]; ok {
			t.Errorf("mutable city runtime file %q was staged into the worker", name)
		}
	}
}

func TestStreamArchiveToPodReturnsProducerError(t *testing.T) {
	want := errors.New("archive read failed")
	ops := newCapturingStageOps()

	err := streamArchiveToPod(
		context.Background(),
		ops,
		"pod",
		"stage",
		"/source",
		"/workspace",
		func(w io.Writer) error {
			tw := tar.NewWriter(w)
			if err := tw.WriteHeader(&tar.Header{Name: "partial", Mode: 0o644, Size: 1}); err != nil {
				return err
			}
			return want
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("streamArchiveToPod error = %v, want %v", err, want)
	}
}

type capturingStageOps struct {
	files               map[string]string
	archiveDestinations []string
	sawPipeReader       bool
}

func newCapturingStageOps() *capturingStageOps {
	return &capturingStageOps{files: make(map[string]string)}
}

func (o *capturingStageOps) createPod(context.Context, *corev1.Pod) (*corev1.Pod, error) {
	return nil, nil
}

func (o *capturingStageOps) getPod(context.Context, string) (*corev1.Pod, error) {
	return &corev1.Pod{
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{},
				},
			}},
		},
	}, nil
}

func (o *capturingStageOps) deletePod(context.Context, string, int64) error {
	return nil
}

func (o *capturingStageOps) listPods(context.Context, string, string) ([]corev1.Pod, error) {
	return nil, nil
}

func (o *capturingStageOps) execInPod(_ context.Context, _, _ string, cmd []string, stdin io.Reader) (string, error) {
	if len(cmd) == 5 && cmd[0] == "tar" && cmd[1] == "xf" && cmd[2] == "-" && cmd[3] == "-C" && stdin != nil {
		_, o.sawPipeReader = stdin.(*io.PipeReader)
		o.archiveDestinations = append(o.archiveDestinations, cmd[4])
		tr := tar.NewReader(stdin)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", err
			}
			if hdr.FileInfo().IsDir() {
				continue
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return "", err
			}
			o.files[path.Join(cmd[4], hdr.Name)] = string(data)
		}
	}
	return "", nil
}

type execReadyOps struct {
	errors       []error
	calls        int
	commands     [][]string
	afterExec    func()
	firstProbeCh chan<- struct{}
}

func (o *execReadyOps) createPod(context.Context, *corev1.Pod) (*corev1.Pod, error) {
	return nil, nil
}

func (o *execReadyOps) getPod(context.Context, string) (*corev1.Pod, error) {
	return nil, nil
}

func (o *execReadyOps) deletePod(context.Context, string, int64) error {
	return nil
}

func (o *execReadyOps) listPods(context.Context, string, string) ([]corev1.Pod, error) {
	return nil, nil
}

func (o *execReadyOps) execInPod(_ context.Context, _, _ string, cmd []string, _ io.Reader) (string, error) {
	o.calls++
	o.commands = append(o.commands, append([]string(nil), cmd...))
	if o.firstProbeCh != nil && o.calls == 1 {
		close(o.firstProbeCh)
	}
	if o.afterExec != nil {
		o.afterExec()
	}
	if o.calls <= len(o.errors) {
		return "", o.errors[o.calls-1]
	}
	return "", nil
}
