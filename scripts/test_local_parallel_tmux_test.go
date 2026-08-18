package scripts_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLocalParallelJobsUseIsolatedTmuxSockets(t *testing.T) {
	script := readLocalParallel(t)
	for _, required := range []string{
		`tmux_tmpdir="$LOCAL_TEST_LOG_DIR/.tmux-$safe_label"`,
		`TMUX_TMPDIR="$tmux_tmpdir"`,
		`tmux -S "$tmux_socket" kill-server`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("test-local-parallel omitted scoped tmux lifecycle fragment %q", required)
		}
	}
	if strings.Contains(script, "tmux kill-server") {
		t.Fatal("test-local-parallel contains unsafe default-server cleanup")
	}
}

func TestLocalParallelPreservesExplicitIsolationRoots(t *testing.T) {
	local := readScript(t, "test-local-parallel")
	for _, required := range []string{"TEST_LOCAL_ZDOTDIR", "TEST_LOCAL_GIT_CONFIG_GLOBAL"} {
		if !strings.Contains(local, required) {
			t.Fatalf("test-local-parallel omitted runner-owned isolation root %q", required)
		}
	}
	for _, name := range []string{"test-integration-shard", "test-go-test-shard"} {
		script := readScript(t, name)
		for _, required := range []string{"ZDOTDIR", "GIT_CONFIG_GLOBAL"} {
			if !strings.Contains(script, required) {
				t.Fatalf("%s omitted nested isolation root %q", name, required)
			}
		}
	}
}

func readLocalParallel(t *testing.T) string {
	return readScript(t, "test-local-parallel")
}

func readScript(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
