package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestEnsureCapsuleCatalogStagesContainedImmutableInputsAndCleansExactly(t *testing.T) {
	root := t.TempDir()
	profiles := writeCapsuleCatalogInput(t, root, "profiles.yaml", []byte("version: 1\n"))
	agent := writeCapsuleCatalogInput(t, root, "agent.yaml", []byte("name: test\n"))
	capsule := testK8sCapsuleLaunch(t)
	capsule.CatalogInputs = []runtime.CapsuleInput{
		{SourcePath: profiles, RelativePath: "profiles.yaml", SHA256: fileDigest(t, profiles), Mode: 0o444},
		{SourcePath: agent, RelativePath: "agents/codex.yaml", SHA256: fileDigest(t, agent), Mode: 0o400},
	}
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)

	ref, created, err := provider.ensureCapsuleCatalog(context.Background(), "ga-session", runtime.Config{Capsule: capsule})
	if err != nil {
		t.Fatalf("ensureCapsuleCatalog: %v", err)
	}
	if !created || ref == nil || ref.Name != capsule.CatalogResourceID || ref.Immutable == nil || !*ref.Immutable {
		t.Fatalf("catalog reference = %+v, created=%t", ref, created)
	}
	if got := string(ref.BinaryData["input-000"]); got != "version: 1\n" {
		t.Fatalf("catalog input-000 = %q", got)
	}
	if got := string(ref.BinaryData["input-001"]); got != "name: test\n" {
		t.Fatalf("catalog input-001 = %q", got)
	}

	reopened, created, err := provider.ensureCapsuleCatalog(context.Background(), "ga-session", runtime.Config{Capsule: capsule})
	if err != nil || created || reopened.UID != ref.UID {
		t.Fatalf("reopen catalog = %+v, created=%t, err=%v", reopened, created, err)
	}
	if err := provider.deleteCapsuleCatalog(context.Background(), ref.Name, ref.UID); err != nil {
		t.Fatalf("deleteCapsuleCatalog: %v", err)
	}
	if _, ok := ops.configMaps[ref.Name]; ok {
		t.Fatalf("catalog ConfigMap %q survived exact cleanup", ref.Name)
	}
}

func TestEnsureCapsuleCatalogRejectsExistingContentConflict(t *testing.T) {
	root := t.TempDir()
	profiles := writeCapsuleCatalogInput(t, root, "profiles.yaml", []byte("version: 1\n"))
	capsule := testK8sCapsuleLaunch(t)
	capsule.CatalogInputs = []runtime.CapsuleInput{{
		SourcePath: profiles, RelativePath: "profiles.yaml", SHA256: fileDigest(t, profiles), Mode: 0o444,
	}}
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	ref, _, err := provider.ensureCapsuleCatalog(context.Background(), "ga-session", runtime.Config{Capsule: capsule})
	if err != nil {
		t.Fatalf("initial ensureCapsuleCatalog: %v", err)
	}
	ops.configMaps[ref.Name].BinaryData["input-000"] = []byte("tampered")
	if _, _, err := provider.ensureCapsuleCatalog(context.Background(), "ga-session", runtime.Config{Capsule: capsule}); err == nil {
		t.Fatal("ensureCapsuleCatalog accepted conflicting immutable content")
	}
}

func TestProviderCapsuleStartAndStopOwnCatalogLifecycle(t *testing.T) {
	root := t.TempDir()
	profiles := writeCapsuleCatalogInput(t, root, "profiles.yaml", []byte("version: 1\n"))
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	capsule.CatalogInputs = []runtime.CapsuleInput{{
		SourcePath: profiles, RelativePath: "profiles.yaml", SHA256: fileDigest(t, profiles), Mode: 0o444,
	}}
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	state, _, err := provider.EnsureCapsuleState(context.Background(), capsule.Key)
	if err != nil {
		t.Fatalf("EnsureCapsuleState: %v", err)
	}
	capsule.State = state
	name := "ga-catalog-lifecycle"
	if err := provider.Start(context.Background(), name, runtime.Config{
		Capsule: capsule,
		Env:     map[string]string{"GC_INSTANCE_TOKEN": "catalog-test"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	configMap, ok := ops.configMaps[capsule.CatalogResourceID]
	if !ok {
		t.Fatalf("Start did not create catalog ConfigMap %q", capsule.CatalogResourceID)
	}
	pod := ops.pods[SanitizeName(name)]
	if pod == nil || pod.Annotations[capsuleCatalogResourceAnnotation] != configMap.Name {
		t.Fatalf("pod catalog annotation = %+v", pod)
	}
	if err := provider.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := ops.configMaps[capsule.CatalogResourceID]; ok {
		t.Fatalf("Stop left catalog ConfigMap %q", capsule.CatalogResourceID)
	}

	if err := provider.Start(context.Background(), name, runtime.Config{
		Capsule: capsule,
		Env:     map[string]string{"GC_INSTANCE_TOKEN": "catalog-test-fenced"},
	}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := provider.StopIfInstanceToken(name, "catalog-test-fenced"); err != nil {
		t.Fatalf("StopIfInstanceToken: %v", err)
	}
	if _, ok := ops.configMaps[capsule.CatalogResourceID]; ok {
		t.Fatalf("fenced stop left catalog ConfigMap %q", capsule.CatalogResourceID)
	}
}

func TestProviderCapsuleStartFailureRemovesReopenedUnreferencedCatalog(t *testing.T) {
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	provider.prebaked = true
	provider.postStartSettle = 0

	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	state, _, err := provider.EnsureCapsuleState(ctx, capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = state
	if _, created, err := provider.ensureCapsuleCatalog(ctx, "ga-reopened-catalog", runtime.Config{Capsule: capsule}); err != nil || !created {
		t.Fatalf("precreate catalog: created=%t err=%v", created, err)
	}

	ops.createErr = errors.New("pod admission denied")
	err = provider.Start(ctx, "ga-reopened-catalog", runtime.Config{Capsule: capsule})
	if err == nil || !strings.Contains(err.Error(), "pod admission denied") {
		t.Fatalf("Start error = %v", err)
	}
	if _, ok := ops.configMaps[capsule.CatalogResourceID]; ok {
		t.Fatalf("failed start left reopened unreferenced catalog ConfigMap %q", capsule.CatalogResourceID)
	}
}

func writeCapsuleCatalogInput(t *testing.T, root, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
