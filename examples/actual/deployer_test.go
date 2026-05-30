// Package actual_test validates the Actual software factory deployer prompt.
//
// RED suite for ga-ozrmfy.2: regression coverage for remote-less deploy gates.
//
// These tests verify that the deployer prompt at
// examples/actual/packs/deployer/prompts/deployer.md contains the required
// local-only release path as defined in the architecture contract (ga-ozrmfy.1).
//
// Tests fail until the builder (ga-ozrmfy.3) adds:
//   - No-remote detection block before push target check
//   - "Process — local-only (no remote)" section
//   - Gate criterion 8 (policy signal confirmed)
//   - Failure routing table for remote-less targets
//   - Evidence recording recipe (gate artifact commit + bead notes + bead close)
package actual_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func exampleDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

func readDeployerPrompt(t *testing.T) string {
	t.Helper()
	path := filepath.Join(exampleDir(), "packs", "deployer", "prompts", "deployer.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading deployer prompt at %s: %v", path, err)
	}
	return string(data)
}

// TestDeployerPromptContainsNoRemoteDetectionBlock verifies the deployer prompt
// has a detection block that checks git remote -v before attempting any push.
//
// Regression: ga-ozrmfy — deployer failed on gc-management (no remotes) because
// it applied the remote-backed push+PR contract without checking for remotes first.
func TestDeployerPromptContainsNoRemoteDetectionBlock(t *testing.T) {
	body := readDeployerPrompt(t)

	required := []string{
		"git remote -v",
		"gc.release_mode",
		"local-only",
		"DEPLOY_MODE",
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("deployer prompt missing no-remote detection content %q", want)
		}
	}
}

// TestDeployerPromptContainsLocalOnlyProcessSection verifies that the deployer
// prompt has a dedicated "Process — local-only" section describing the local merge
// path for remote-less repos that have declared gc.release_mode=local-only.
func TestDeployerPromptContainsLocalOnlyProcessSection(t *testing.T) {
	body := readDeployerPrompt(t)

	if !strings.Contains(body, "local-only") || !strings.Contains(body, "Process") {
		t.Errorf("deployer prompt missing 'Process — local-only' section")
	}

	// Must include the local merge recipe.
	if !strings.Contains(body, "git merge --ff-only") {
		t.Errorf("deployer prompt missing 'git merge --ff-only' recipe in local-only section")
	}
}

// TestDeployerPromptContainsGateCriterion8 verifies the deployer prompt adds
// criterion 8 — "policy signal confirmed" — to the gate criteria table and
// that it is checked BEFORE criteria 1–7.
func TestDeployerPromptContainsGateCriterion8(t *testing.T) {
	body := readDeployerPrompt(t)

	if !strings.Contains(body, "criterion 8") && !strings.Contains(body, "Criterion 8") {
		t.Errorf("deployer prompt missing gate criterion 8 (policy signal confirmed)")
	}

	// Criterion 8 must be evaluated first (before criteria 1–7).
	idx8 := strings.Index(body, "criterion 8")
	if idx8 == -1 {
		idx8 = strings.Index(body, "Criterion 8")
	}
	idx1 := strings.Index(body, "criterion 1")
	if idx1 == -1 {
		idx1 = strings.Index(body, "Criterion 1")
	}
	if idx8 != -1 && idx1 != -1 && idx8 > idx1 {
		t.Errorf("criterion 8 appears after criterion 1 in prompt; it must be evaluated first")
	}
}

// TestDeployerPromptLocalCriterion6UsesLocalMain verifies that the local-only
// gate adaptation for criterion 6 uses "git merge-tree main" against the local
// HEAD, not "git merge-tree origin/main" (which would fail on remote-less repos).
func TestDeployerPromptLocalCriterion6UsesLocalMain(t *testing.T) {
	body := readDeployerPrompt(t)

	if !strings.Contains(body, "git merge-tree main") {
		t.Errorf("deployer prompt missing 'git merge-tree main' (local-only criterion 6 adaptation)")
	}
}

// TestDeployerPromptManifestFallbackSignal verifies the deployer prompt checks
// the rig manifest (release_mode = "local") as a fallback policy signal when
// gc.release_mode is not set in bead metadata.
func TestDeployerPromptManifestFallbackSignal(t *testing.T) {
	body := readDeployerPrompt(t)

	if !strings.Contains(body, "release_mode") {
		t.Errorf("deployer prompt missing manifest release_mode fallback signal check")
	}
}

// TestDeployerPromptMissingSignalRoutesToPM verifies the deployer prompt routes
// to PM when no remote is present AND no policy signal is declared, preventing
// silent local merges on misconfigured repos.
func TestDeployerPromptMissingSignalRoutesToPM(t *testing.T) {
	body := readDeployerPrompt(t)

	required := []string{
		"needs-pm",
		"gc.release_mode",
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("deployer prompt missing PM routing content %q for no-signal case", want)
		}
	}
}

// TestDeployerPromptGateArtifactWrittenBeforeMerge verifies the deployer prompt
// requires the gate artifact to be written BEFORE the merge executes, and that
// the gate artifact commit happens AFTER the merge (so evidence is on main).
func TestDeployerPromptGateArtifactWrittenBeforeMerge(t *testing.T) {
	body := readDeployerPrompt(t)

	// The local-only section must include the merge recipe AND the gate artifact
	// commit. Both are required for this ordering check to be meaningful.
	if !strings.Contains(body, "git merge --ff-only") {
		t.Errorf("deployer prompt missing 'git merge --ff-only' recipe (local-only section not yet present)")
	}
	if !strings.Contains(body, "git add release-gates") {
		t.Errorf("deployer prompt missing 'git add release-gates' (gate artifact commit not yet present)")
	}

	// Gate artifact write (evidence checklist) must precede the merge.
	// Gate artifact commit (on main) must follow the merge.
	writeIdx := strings.Index(body, "release-gates/")
	mergeIdx := strings.Index(body, "git merge --ff-only")
	commitIdx := strings.Index(body, "git add release-gates")
	if writeIdx != -1 && mergeIdx != -1 && writeIdx > mergeIdx {
		t.Errorf("gate artifact write appears after merge recipe; artifact must be written (as evidence) before merge")
	}
	if commitIdx != -1 && mergeIdx != -1 && commitIdx < mergeIdx {
		t.Errorf("gate artifact commit appears before merge recipe; artifact must be committed to main AFTER merge")
	}
}

// TestDeployerPromptEvidenceRecording verifies the deployer prompt records all
// required post-merge evidence: gate artifact commit, final SHA in bead notes,
// and bead close.
func TestDeployerPromptEvidenceRecording(t *testing.T) {
	body := readDeployerPrompt(t)

	checks := []struct {
		want string
		desc string
	}{
		{"bd update", "bd update call for bead notes/close"},
		{"append-notes", "--append-notes for final SHA recording"},
		{"final main", "final main SHA reference in notes recipe"},
		{"git add release-gates", "gate artifact staged before commit"},
	}
	for _, c := range checks {
		if !strings.Contains(body, c.want) {
			t.Errorf("deployer prompt missing evidence recording: %s (wanted %q)", c.desc, c.want)
		}
	}
}

// TestDeployerPromptFailureRoutingTechToBuilder verifies the deployer prompt
// routes technical failures (test fail, merge conflict) to the builder via
// ready-to-build label — NOT to PM.
func TestDeployerPromptFailureRoutingTechToBuilder(t *testing.T) {
	body := readDeployerPrompt(t)

	if !strings.Contains(body, "ready-to-build") {
		t.Errorf("deployer prompt missing 'ready-to-build' label for tech failure routing")
	}

	// Merge conflict should route to builder, not PM.
	if !strings.Contains(body, "merge conflict") && !strings.Contains(body, "conflict") {
		t.Errorf("deployer prompt missing merge conflict → builder routing description")
	}
}

// TestDeployerPromptRollupShipLocalOnlyOutOfScope verifies the deployer prompt
// explicitly notes that rollup-ship combined with local-only is out of scope
// and should route to PM.
func TestDeployerPromptRollupShipLocalOnlyOutOfScope(t *testing.T) {
	body := readDeployerPrompt(t)

	hasRollup := strings.Contains(body, "rollup") || strings.Contains(body, "rollup-ship")
	hasOutOfScope := strings.Contains(body, "out of scope") || strings.Contains(body, "not supported")
	if !hasRollup || !hasOutOfScope {
		t.Errorf("deployer prompt missing rollup-ship + local-only out-of-scope note")
	}
}

// TestDeployerRegressionGaOzrmfyScenario is the named regression scenario from
// ga-ozrmfy: the deployer receives a bead for gc-management (a repo with no
// remotes) and gc.release_mode=local-only is set. The deployer should complete
// successfully with a gate artifact and final SHA, with no push and no PR.
//
// This test verifies that the prompt describes all required elements of this path.
func TestDeployerRegressionGaOzrmfyScenario(t *testing.T) {
	body := readDeployerPrompt(t)

	// The deployer prompt must describe the full local-only success path.
	successPath := []string{
		"git remote -v",       // detect no remotes
		"gc.release_mode",     // check policy signal
		"local-only",          // recognize local-only declaration
		"git merge --ff-only", // local merge recipe
		"release-gates/",      // gate artifact path
		"final main",          // final SHA recording
		"bd update",           // bead notes update
	}
	for _, want := range successPath {
		if !strings.Contains(body, want) {
			t.Errorf("regression ga-ozrmfy: deployer prompt missing %q for local-only success path", want)
		}
	}

	// Must NOT treat missing remote as a silent local-merge default.
	// The prompt must require the explicit policy signal.
	if !strings.Contains(body, "needs-pm") {
		t.Errorf("regression ga-ozrmfy: deployer prompt missing PM routing for missing policy signal case")
	}
}
