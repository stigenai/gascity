package omnigent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var versionOutputPattern = regexp.MustCompile(`^omnigent\s+(\S+)(?:\s+\(([0-9a-f]{8,40})(?:,|\)))?`)

const pinRemediation = "install the exact pinned Omnigent build or update the pin only after a compatibility audit"

// VerifiedExecutable is an exact executable identity safe to use for one
// immediate sidecar start. Callers re-verify on every later restart.
type VerifiedExecutable struct {
	Path          string
	SHA256        string
	VersionOutput string
}

// VerifyExecutable resolves Pin.Executable through PATH and verifies its file
// digest and machine-readable version output.
func VerifyExecutable(ctx context.Context, pin Pin) (VerifiedExecutable, error) {
	if err := validatePin(pin); err != nil {
		return VerifiedExecutable{}, err
	}
	path, err := exec.LookPath(pin.Executable)
	if err != nil {
		return VerifiedExecutable{}, fmt.Errorf("find pinned omnigent executable: not found; %s", pinRemediation)
	}
	return VerifyExecutableAt(ctx, pin, path)
}

// VerifyExecutableAt verifies one explicit Omnigent executable path. Symlinks
// are resolved and the resolved target is returned so the launched path is the
// same file whose bytes were hashed.
func VerifyExecutableAt(ctx context.Context, pin Pin, path string) (VerifiedExecutable, error) {
	if err := validatePin(pin); err != nil {
		return VerifiedExecutable{}, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return VerifiedExecutable{}, errors.New("omnigent executable path is required")
	}
	if strings.IndexFunc(path, unicode.IsSpace) >= 0 {
		return VerifiedExecutable{}, errors.New("omnigent executable must use a path without whitespace; move or link the exact verified executable to a path without whitespace")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return VerifiedExecutable{}, fmt.Errorf("resolve omnigent executable path: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return VerifiedExecutable{}, fmt.Errorf("resolve omnigent executable: path is unavailable; %s", pinRemediation)
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return VerifiedExecutable{}, fmt.Errorf("inspect omnigent executable: path is unavailable; %s", pinRemediation)
	}
	if !info.Mode().IsRegular() {
		return VerifiedExecutable{}, fmt.Errorf("omnigent executable is not a regular file; %s", pinRemediation)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return VerifiedExecutable{}, fmt.Errorf("omnigent executable is not executable; %s", pinRemediation)
	}
	f, err := os.Open(resolvedPath)
	if err != nil {
		return VerifiedExecutable{}, fmt.Errorf("open omnigent executable for verification: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, f)
	closeErr := f.Close()
	if copyErr != nil {
		return VerifiedExecutable{}, fmt.Errorf("hash omnigent executable: %w", copyErr)
	}
	if closeErr != nil {
		return VerifiedExecutable{}, fmt.Errorf("close omnigent executable after verification: %w", closeErr)
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actualDigest != pin.SHA256 {
		return VerifiedExecutable{}, fmt.Errorf("omnigent executable digest mismatch: got %s, want %s; %s", actualDigest, pin.SHA256, pinRemediation)
	}

	verifyCtx := ctx
	var cancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		verifyCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(verifyCtx, resolvedPath, "--version")
	cmd.Env = verificationEnvironment()
	output, err := cmd.CombinedOutput()
	if err != nil {
		if verifyCtx.Err() != nil {
			return VerifiedExecutable{}, fmt.Errorf("query omnigent version: %w", verifyCtx.Err())
		}
		return VerifiedExecutable{}, fmt.Errorf("query omnigent version: %w", err)
	}
	versionOutput := strings.TrimSpace(string(output))
	matches := versionOutputPattern.FindStringSubmatch(versionOutput)
	if matches == nil {
		return VerifiedExecutable{}, fmt.Errorf("omnigent --version returned unsupported output; %s", pinRemediation)
	}
	if matches[1] != pin.PackageVersion {
		return VerifiedExecutable{}, fmt.Errorf("omnigent version mismatch: got %q, want %q; %s", matches[1], pin.PackageVersion, pinRemediation)
	}
	if matches[2] == "" {
		return VerifiedExecutable{}, fmt.Errorf("omnigent --version does not report a build commit; %s", pinRemediation)
	}
	if !strings.HasPrefix(pin.Commit, matches[2]) {
		return VerifiedExecutable{}, fmt.Errorf("omnigent build commit mismatch: got %q, want prefix of %q; %s", matches[2], pin.Commit, pinRemediation)
	}
	return VerifiedExecutable{
		Path:          resolvedPath,
		SHA256:        actualDigest,
		VersionOutput: versionOutput,
	}, nil
}

func verificationEnvironment() []string {
	env := []string{
		"OMNIGENT_NO_UPDATE_CHECK=1",
		"OMNIGENT_DISABLE_TELEMETRY=true",
		"OMNIGENT_TELEMETRY_ENABLED=0",
		"OMNIGENT_OTEL_CAPTURE_CONTENT=0",
		"DO_NOT_TRACK=1",
	}
	for _, key := range []string{"HOME", "LANG", "LC_ALL", "PATH", "SYSTEMROOT", "WINDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func boundedText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
