// Package actual_test — shell detection script tests for ga-ozrmfy.2.
//
// These tests extract the no-remote detection bash block from the deployer
// prompt and exercise it against controlled git/bd environments.
//
// The tests fail until the builder (ga-ozrmfy.3) adds the detection block to
// examples/actual/packs/deployer/prompts/deployer.md.
package actual_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bashBlockRe matches a fenced ```bash block from the deployer prompt.
// We look for the block that contains "git remote -v" as the no-remote
// detection recipe.
var bashBlockRe = regexp.MustCompile("(?s)```bash\n(.*?)```")

// extractDetectionBlock finds the no-remote detection bash snippet from the
// deployer prompt. Fails the test if the block cannot be found — this is the
// RED condition before the builder adds the detection section.
func extractDetectionBlock(t *testing.T, promptBody string) string {
	t.Helper()
	matches := bashBlockRe.FindAllStringSubmatch(promptBody, -1)
	for _, m := range matches {
		code := m[1]
		if strings.Contains(code, "git remote -v") &&
			strings.Contains(code, "gc.release_mode") &&
			strings.Contains(code, "DEPLOY_MODE") {
			return code
		}
	}
	t.Fatalf("deployer prompt missing no-remote detection bash block (ga-ozrmfy.3 not yet implemented)")
	return ""
}

// writeExecutable creates a file with the given content and makes it executable.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing executable %s: %v", path, err)
	}
}

// setupGitStub creates a git stub that returns the given remote-v output.
// Empty remoteV = no remotes configured (the local-only case).
func setupGitStub(t *testing.T, binDir, remoteV string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"remote -v"*)
    printf '%%s\n' %q
    exit 0
    ;;
  *"status --porcelain"*)
    exit 0
    ;;
  *"merge-tree"*)
    exit 0
    ;;
  *"merge --ff-only"*)
    exit 0
    ;;
  *"rev-parse HEAD"*)
    printf 'abc1234def5678abc1234def5678abc1234def56\n'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, remoteV)
	writeExecutable(t, filepath.Join(binDir, "git"), script)
}

// setupBdStub creates a bd stub that returns the given release_mode when
// queried for bead metadata.
func setupBdStub(t *testing.T, binDir, releaseMode string) {
	t.Helper()
	var jsonOut string
	if releaseMode != "" {
		jsonOut = fmt.Sprintf(`{"id":"test-bead","metadata":{"gc.release_mode":%q}}`, releaseMode)
	} else {
		jsonOut = `{"id":"test-bead","metadata":{}}`
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"--json"*)
    printf '%%s\n' %q
    exit 0
    ;;
  *"update"*)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, jsonOut)
	writeExecutable(t, filepath.Join(binDir, "bd"), script)
}

// setupJqStub creates a minimal jq stub that extracts gc.release_mode from
// the JSON piped into it.
func setupJqStub(t *testing.T, binDir, releaseMode string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
# Minimal jq stub: returns the release_mode value passed at test setup time.
printf '%%s\n' %q
`, releaseMode)
	writeExecutable(t, filepath.Join(binDir, "jq"), script)
}

// runDetectionScript runs the detection bash snippet in a controlled environment
// and returns the value of DEPLOY_MODE set by the script.
func runDetectionScript(t *testing.T, script, binDir, beadID, rigRoot string) (deployMode string, exitCode int) {
	t.Helper()

	// Wrap the detection snippet in a harness that prints DEPLOY_MODE.
	harness := fmt.Sprintf(`#!/bin/sh
set -e
BEAD_ID=%q
GC_RIG_ROOT=%q
%s
printf 'DEPLOY_MODE=%%s\n' "${DEPLOY_MODE:-}"
`, beadID, rigRoot, script)

	scriptPath := filepath.Join(t.TempDir(), "detect.sh")
	writeExecutable(t, scriptPath, harness)

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+t.TempDir(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
	)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "DEPLOY_MODE=") {
			deployMode = strings.TrimPrefix(line, "DEPLOY_MODE=")
			break
		}
	}
	return deployMode, exitCode
}

// TestDeployerDetectionLocalOnlyBeadMetadata verifies the detection algorithm
// selects the local-only path when gc.release_mode=local-only is in bead metadata
// and git remote -v returns empty.
func TestDeployerDetectionLocalOnlyBeadMetadata(t *testing.T) {
	body := readDeployerPrompt(t)
	script := extractDetectionBlock(t, body)

	binDir := t.TempDir()
	rigRoot := t.TempDir()
	setupGitStub(t, binDir, "")          // empty = no remotes
	setupBdStub(t, binDir, "local-only") // bead has gc.release_mode=local-only
	setupJqStub(t, binDir, "local-only")

	deployMode, _ := runDetectionScript(t, script, binDir, "ga-test-bead", rigRoot)
	if deployMode != "local" {
		t.Errorf("detection with gc.release_mode=local-only: DEPLOY_MODE=%q, want %q", deployMode, "local")
	}
}

// TestDeployerDetectionManifestFallback verifies the detection algorithm
// selects the local-only path when the rig manifest declares release_mode = "local"
// and gc.release_mode is absent from bead metadata.
func TestDeployerDetectionManifestFallback(t *testing.T) {
	body := readDeployerPrompt(t)
	script := extractDetectionBlock(t, body)

	binDir := t.TempDir()
	rigRoot := t.TempDir()
	setupGitStub(t, binDir, "") // no remotes
	setupBdStub(t, binDir, "")  // no gc.release_mode in bead
	setupJqStub(t, binDir, "")  // jq returns empty for gc.release_mode

	// Create rig manifest with release_mode = "local".
	manifestDir := filepath.Join(rigRoot, ".actual")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	manifestContent := `# Rig manifest
release_mode = "local"
`
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.md"), []byte(manifestContent), 0o644); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}

	deployMode, _ := runDetectionScript(t, script, binDir, "ga-test-bead", rigRoot)
	if deployMode != "local" {
		t.Errorf("detection with manifest release_mode=local: DEPLOY_MODE=%q, want %q", deployMode, "local")
	}
}

// TestDeployerDetectionMissingSignalFailsGate verifies that when a repo has no
// remotes but no policy signal is declared (no bead metadata, no manifest), the
// detection algorithm fails (non-zero exit) rather than silently choosing local merge.
//
// This is the safety gate against misconfigured repos: no signal = PM routing.
func TestDeployerDetectionMissingSignalFailsGate(t *testing.T) {
	body := readDeployerPrompt(t)
	script := extractDetectionBlock(t, body)

	binDir := t.TempDir()
	rigRoot := t.TempDir()
	setupGitStub(t, binDir, "") // no remotes
	setupBdStub(t, binDir, "")  // no gc.release_mode
	setupJqStub(t, binDir, "")  // no release_mode from bead JSON

	// No manifest file in rigRoot — no policy signal at all.

	_, exitCode := runDetectionScript(t, script, binDir, "ga-test-bead", rigRoot)
	if exitCode == 0 {
		t.Errorf("detection with no remotes and no signal: expected non-zero exit (gate fail), got exit 0")
	}
}

// TestDeployerDetectionRemoteBackedPathUnchanged verifies that when the target
// repo HAS a push-capable remote, the detection algorithm does NOT select the
// local-only path, preserving the existing remote-backed push+PR behavior.
func TestDeployerDetectionRemoteBackedPathUnchanged(t *testing.T) {
	body := readDeployerPrompt(t)
	script := extractDetectionBlock(t, body)

	binDir := t.TempDir()
	rigRoot := t.TempDir()
	// Remote-backed: git remote -v returns non-empty output.
	setupGitStub(t, binDir, "origin\thttps://github.com/example/repo.git (fetch)\norigin\thttps://github.com/example/repo.git (push)")
	setupBdStub(t, binDir, "local-only") // even if bead says local-only, remote presence wins
	setupJqStub(t, binDir, "local-only")

	deployMode, _ := runDetectionScript(t, script, binDir, "ga-test-bead", rigRoot)
	if deployMode == "local" {
		t.Errorf("detection with existing remotes: DEPLOY_MODE=%q, want non-local (remote-backed path)", deployMode)
	}
}

// TestDeployerRegressionGaOzrmfyNamedScenario is the named regression scenario
// from ga-ozrmfy. The deployer receives:
//   - A repo with no remotes (like gc-management)
//   - A bead with gc.release_mode=local-only
//
// Expected: detection selects local path (DEPLOY_MODE=local), does NOT fail gate.
// This is the scenario that originally caused a deploy failure.
func TestDeployerRegressionGaOzrmfyNamedScenario(t *testing.T) {
	body := readDeployerPrompt(t)
	script := extractDetectionBlock(t, body)

	binDir := t.TempDir()
	rigRoot := t.TempDir()

	// Simulate gc-management: a git repo with no remotes.
	setupGitStub(t, binDir, "")

	// Mayor routed the bead with gc.release_mode=local-only per the contract.
	setupBdStub(t, binDir, "local-only")
	setupJqStub(t, binDir, "local-only")

	deployMode, exitCode := runDetectionScript(t, script, binDir, "ga-ozrmfy", rigRoot)

	if exitCode != 0 {
		t.Errorf("ga-ozrmfy regression: detection failed with exit %d; deployer would incorrectly fail deploy", exitCode)
	}
	if deployMode != "local" {
		t.Errorf("ga-ozrmfy regression: DEPLOY_MODE=%q, want %q; gc-management local merge path not selected", deployMode, "local")
	}
}
