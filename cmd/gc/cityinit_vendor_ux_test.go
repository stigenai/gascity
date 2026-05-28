package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitFromDirVendorCLISilentWhenNoExternalDeps verifies that gc init --from
// emits no vendoring summary when the source city has no external pack deps.
func TestInitFromDirVendorCLISilentWhenNoExternalDeps(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	cityPath := filepath.Join(dir, "city")

	writeInitVendorUXFile(t, srcDir, "city.toml", "[workspace]\nname = \"src\"\nprovider = \"claude\"\n")
	// All imports are local (inside srcDir) — none escape, so nothing is vendored.
	writeInitVendorUXFile(t, srcDir, "pack.toml", "[pack]\nname = \"src\"\nschema = 2\n")
	writeInitVendorUXFile(t, srcDir, "packs/local/pack.toml", "[pack]\nname = \"local\"\nschema = 2\n")

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "Vendored") {
		t.Fatalf("stdout should not contain vendoring summary when no external deps:\n%s", stdout.String())
	}
}

// TestInitFromDirVendorCLISingularSummaryOnOneDep verifies that gc init --from
// emits the singular "Vendored 1 local pack dependency: packs/vendor/<name>"
// summary line before the "Welcome to Gas City!" banner when exactly one
// external pack dep is vendored.
func TestInitFromDirVendorCLISingularSummaryOnOneDep(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	cityPath := filepath.Join(dir, "city")
	depDir := filepath.Join(dir, "dep")

	writeInitVendorUXFile(t, depDir, "pack.toml", "[pack]\nname = \"dep\"\nschema = 2\n")
	writeInitVendorUXFile(t, srcDir, "city.toml", "[workspace]\nname = \"src\"\nprovider = \"claude\"\n")
	writeInitVendorUXFile(t, srcDir, "pack.toml", "[pack]\nname = \"src\"\nschema = 2\n\n[imports.dep]\nsource = \"../dep\"\n")

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	const wantSummary = "Vendored 1 local pack dependency: packs/vendor/dep"
	out := stdout.String()
	if !strings.Contains(out, wantSummary) {
		t.Fatalf("stdout missing singular vendoring summary %q:\n%s", wantSummary, out)
	}
	summaryIdx := strings.Index(out, wantSummary)
	welcomeIdx := strings.Index(out, "Welcome to Gas City!")
	if welcomeIdx >= 0 && summaryIdx >= welcomeIdx {
		t.Fatalf("vendor summary (at %d) must appear before 'Welcome to Gas City!' (at %d):\n%s", summaryIdx, welcomeIdx, out)
	}
}

// TestInitFromDirVendorCLIPluralSortedSummaryOnMultipleDeps verifies that
// gc init --from emits the plural "Vendored N local pack dependencies: n1, n2, ..."
// summary line (sorted, comma-joined basenames) before the welcome banner when
// more than one external pack dep is vendored.
func TestInitFromDirVendorCLIPluralSortedSummaryOnMultipleDeps(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	cityPath := filepath.Join(dir, "city")

	writeInitVendorUXFile(t, filepath.Join(dir, "alpha"), "pack.toml", "[pack]\nname = \"alpha\"\nschema = 2\n")
	writeInitVendorUXFile(t, filepath.Join(dir, "beta"), "pack.toml", "[pack]\nname = \"beta\"\nschema = 2\n")
	writeInitVendorUXFile(t, srcDir, "city.toml", "[workspace]\nname = \"src\"\nprovider = \"claude\"\n")
	writeInitVendorUXFile(t, srcDir, "pack.toml",
		"[pack]\nname = \"src\"\nschema = 2\n\n[imports.alpha]\nsource = \"../alpha\"\n\n[imports.beta]\nsource = \"../beta\"\n")

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	const wantSummary = "Vendored 2 local pack dependencies: alpha, beta"
	out := stdout.String()
	if !strings.Contains(out, wantSummary) {
		t.Fatalf("stdout missing plural vendoring summary %q:\n%s", wantSummary, out)
	}
	summaryIdx := strings.Index(out, wantSummary)
	welcomeIdx := strings.Index(out, "Welcome to Gas City!")
	if welcomeIdx >= 0 && summaryIdx >= welcomeIdx {
		t.Fatalf("vendor summary (at %d) must appear before 'Welcome to Gas City!' (at %d):\n%s", summaryIdx, welcomeIdx, out)
	}
}

// TestInitFromDirVendorCLIRunawayGuardNonZeroNoPartialOutput verifies that
// gc init --from returns non-zero and emits no partial vendoring summary when
// the 50-pack runaway guard triggers.
func TestInitFromDirVendorCLIRunawayGuardNonZeroNoPartialOutput(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	cityPath := filepath.Join(dir, "city")

	writeInitVendorUXFile(t, srcDir, "city.toml", "[workspace]\nname = \"src\"\nprovider = \"claude\"\n")
	writeInitVendorUXFile(t, srcDir, "pack.toml",
		"[pack]\nname = \"src\"\nschema = 2\n\n[imports.p00]\nsource = \"../p00\"\n")
	// Chain of 51 packs (p00 → p01 → ... → p49 → p50) triggers the 50-pack guard.
	for i := 0; i < 51; i++ {
		name := fmt.Sprintf("p%02d", i)
		content := fmt.Sprintf("[pack]\nname = %q\nschema = 2\n", name)
		if i < 50 {
			next := fmt.Sprintf("p%02d", i+1)
			content += fmt.Sprintf("\n[imports.%s]\nsource = \"../%s\"\n", next, next)
		}
		writeInitVendorUXFile(t, filepath.Join(dir, name), "pack.toml", content)
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doInitFromDir should fail with runaway graph guard, got code=0;\nstdout: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "Vendored") {
		t.Fatalf("stdout must not contain partial vendoring summary when runaway guard triggers:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "dependency graph exceeds") {
		t.Fatalf("stderr missing runaway guard message:\n%s", stderr.String())
	}
}

// TestInitFromDirVendorCLIVendoringErrorHasContextPrefix verifies that
// gc init --from emits "gc init --from: vendoring local pack deps:" in stderr
// when vendoring a referenced pack fails.
func TestInitFromDirVendorCLIVendoringErrorHasContextPrefix(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")

	writeInitVendorUXFile(t, srcDir, "city.toml", "[workspace]\nname = \"src\"\nprovider = \"claude\"\n")
	writeInitVendorUXFile(t, srcDir, "pack.toml",
		"[pack]\nname = \"src\"\nschema = 2\n\n[imports.missing]\nsource = \"../missing\"\n")

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, filepath.Join(dir, "city"), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doInitFromDir should fail when referenced pack does not exist, got code=0")
	}
	if !strings.Contains(stderr.String(), "gc init --from: vendoring local pack deps:") {
		t.Fatalf("stderr missing vendoring error context prefix:\n%s", stderr.String())
	}
}

// TestInitFromDirVendorCLINoTransitiveAnnotationInOutput verifies that
// gc init --from never emits "← dep of" transitive-dep annotations in stdout
// (those are a v2 feature excluded from MVP).
func TestInitFromDirVendorCLINoTransitiveAnnotationInOutput(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	cityPath := filepath.Join(dir, "city")

	// dep has a transitive dep — both get vendored, but the summary must not
	// annotate transitives with "← dep of".
	writeInitVendorUXFile(t, filepath.Join(dir, "transitive"), "pack.toml",
		"[pack]\nname = \"transitive\"\nschema = 2\n")
	writeInitVendorUXFile(t, filepath.Join(dir, "dep"), "pack.toml",
		"[pack]\nname = \"dep\"\nschema = 2\n\n[imports.transitive]\nsource = \"../transitive\"\n")
	writeInitVendorUXFile(t, srcDir, "city.toml", "[workspace]\nname = \"src\"\nprovider = \"claude\"\n")
	writeInitVendorUXFile(t, srcDir, "pack.toml",
		"[pack]\nname = \"src\"\nschema = 2\n\n[imports.dep]\nsource = \"../dep\"\n")

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "← dep of") {
		t.Fatalf("stdout must not contain transitive-dep annotation (v2 feature not in MVP):\n%s", stdout.String())
	}
}

// writeInitVendorUXFile creates root/rel with the given content, creating
// intermediate directories as needed.
func writeInitVendorUXFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
