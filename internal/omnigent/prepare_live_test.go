package omnigent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreparePinnedOmnigentLiveSidecar is an opt-in preflight for the exact
// pinned executable, catalog, and provider-projected environment used by a
// live sidecar. It starts no process and records no credential values.
func TestPreparePinnedOmnigentLiveSidecar(t *testing.T) {
	stateRoot := filepath.Clean(strings.TrimSpace(os.Getenv("GC_OMNIGENT_PREPARE_STATE_ROOT")))
	catalogPath := filepath.Clean(strings.TrimSpace(os.Getenv("GC_OMNIGENT_PREPARE_CATALOG")))
	socketPath := filepath.Clean(strings.TrimSpace(os.Getenv("GC_OMNIGENT_PREPARE_SOCKET")))
	expectedEnvironment := strings.TrimSpace(os.Getenv("GC_OMNIGENT_PREPARE_ENVIRONMENT"))
	if !filepath.IsAbs(stateRoot) || !filepath.IsAbs(catalogPath) || !filepath.IsAbs(socketPath) || expectedEnvironment == "" {
		t.Skip("set absolute GC_OMNIGENT_PREPARE_STATE_ROOT, GC_OMNIGENT_PREPARE_CATALOG, GC_OMNIGENT_PREPARE_SOCKET, and GC_OMNIGENT_PREPARE_ENVIRONMENT")
	}
	prepared, err := PrepareSidecar(context.Background(), SidecarConfig{
		StateRoot:   stateRoot,
		CatalogPath: catalogPath,
		SocketPath:  socketPath,
	}, 43123)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(prepared.Catalog.EnvironmentNames(), []string{expectedEnvironment}) {
		t.Fatalf("prepared environment names = %v", prepared.Catalog.EnvironmentNames())
	}
	if !environmentContainsName(prepared.Plan.Env, "OMNIGENT_RUNNER_ENV_PASSTHROUGH") {
		t.Fatal("prepared child environment omits Omnigent runner passthrough declaration")
	}
}

func environmentContainsName(environment []string, name string) bool {
	for _, entry := range environment {
		if key, _, ok := strings.Cut(entry, "="); ok && key == name {
			return true
		}
	}
	return false
}
