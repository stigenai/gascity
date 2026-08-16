package ssh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// CapsulePreflightFailureKind is the stable, non-secret class of a remote SSH
// capsule prerequisite failure.
type CapsulePreflightFailureKind string

const (
	// CapsulePreflightInvalidConfig rejects an invalid non-secret launch plan.
	CapsulePreflightInvalidConfig CapsulePreflightFailureKind = "invalid-config"
	// CapsulePreflightTransport reports an inconclusive remote transport call.
	CapsulePreflightTransport CapsulePreflightFailureKind = "transport"
	// CapsulePreflightUnsupportedPlatform rejects a non-Linux/Darwin host.
	CapsulePreflightUnsupportedPlatform CapsulePreflightFailureKind = "unsupported-platform"
	// CapsulePreflightUnsupportedShell rejects incompatible remote sh behavior.
	CapsulePreflightUnsupportedShell CapsulePreflightFailureKind = "unsupported-shell"
	// CapsulePreflightMissingBinary reports a missing or incompatible tool.
	CapsulePreflightMissingBinary CapsulePreflightFailureKind = "missing-binary"
	// CapsulePreflightPinMismatch reports an Omnigent digest/version mismatch.
	CapsulePreflightPinMismatch CapsulePreflightFailureKind = "pin-mismatch"
	// CapsulePreflightUnwritablePath rejects unusable workspace/state roots.
	CapsulePreflightUnwritablePath CapsulePreflightFailureKind = "unwritable-path"
	// CapsulePreflightMissingProfileAuth reports unavailable typed profile auth.
	CapsulePreflightMissingProfileAuth CapsulePreflightFailureKind = "missing-profile-auth"
)

// CapsulePreflightError identifies why a remote capsule was rejected without
// including credential values or provider-specific credential paths.
type CapsulePreflightError struct {
	Kind        CapsulePreflightFailureKind
	Requirement string
	Err         error
}

func (e *CapsulePreflightError) Error() string {
	message := "ssh capsule preflight " + string(e.Kind)
	if e.Requirement != "" {
		message += ": " + e.Requirement
	}
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	return message
}

// Unwrap preserves transport and validation sentinels for controller policy.
func (e *CapsulePreflightError) Unwrap() error { return e.Err }

var (
	remoteTmuxVersionPattern = regexp.MustCompile(`^tmux\s+(\d+)\.(\d+)`)
	remotePinVersionPattern  = regexp.MustCompile(`^omnigent\s+(\S+)(?:\s+\(([0-9a-f]{8,40})(?:,|\)))?`)
)

func (p *Provider) preflightCapsule(ctx context.Context, cfg runtime.Config) error {
	capsule := cfg.Capsule
	if capsule == nil {
		return nil
	}
	if err := capsule.Validate(); err != nil {
		return &CapsulePreflightError{Kind: CapsulePreflightInvalidConfig, Requirement: "capsule launch plan", Err: err}
	}
	refs, err := runtime.SelectSecretReferences(runtime.SecretProviderSSH, cfg.SecretReferences)
	if err != nil {
		return &CapsulePreflightError{Kind: CapsulePreflightMissingProfileAuth, Requirement: "typed SSH profile references", Err: err}
	}

	platformBytes, err := p.preflightCommand(ctx, CapsulePreflightUnsupportedPlatform, "remote platform", []string{"uname", "-s"})
	if err != nil {
		return err
	}
	platform := strings.TrimSpace(string(platformBytes))
	if platform != "Linux" && platform != "Darwin" {
		return &CapsulePreflightError{Kind: CapsulePreflightUnsupportedPlatform, Requirement: fmt.Sprintf("platform %q is not supported", platform)}
	}
	if _, err := p.preflightCommand(ctx, CapsulePreflightUnsupportedShell, "POSIX sh argument handling", []string{
		"sh", "-c", `test "$1" = gc-shell-probe`, "gc-shell-probe", "gc-shell-probe",
	}); err != nil {
		return err
	}

	tmuxPath, err := p.lookupRemoteBinary(ctx, "tmux", "tmux")
	if err != nil {
		return err
	}
	gcPath, err := p.lookupRemoteBinary(ctx, "gc", "gc")
	if err != nil {
		return err
	}
	omnigentPath, err := p.lookupRemoteBinary(ctx, "omnigent", capsule.ExecutablePin.Executable)
	if err != nil {
		return err
	}
	tmuxVersion, err := p.preflightCommand(ctx, CapsulePreflightMissingBinary, "tmux version", []string{tmuxPath, "-V"})
	if err != nil {
		return err
	}
	if !supportedRemoteTmuxVersion(string(tmuxVersion)) {
		return &CapsulePreflightError{Kind: CapsulePreflightMissingBinary, Requirement: "tmux >= 3.2 is required"}
	}
	if _, err := p.preflightCommand(ctx, CapsulePreflightMissingBinary, "compatible remote gc omnigent command", []string{
		gcPath, "omnigent", "attach", "--help",
	}); err != nil {
		return err
	}

	hashCommand := []string{"sha256sum", "--", omnigentPath}
	if platform == "Darwin" {
		hashCommand = []string{"shasum", "-a", "256", "--", omnigentPath}
	}
	digestOutput, err := p.preflightCommand(ctx, CapsulePreflightPinMismatch, "pinned omnigent digest", hashCommand)
	if err != nil {
		return err
	}
	digestFields := strings.Fields(string(digestOutput))
	if len(digestFields) == 0 || "sha256:"+digestFields[0] != capsule.ExecutablePin.SHA256 {
		return &CapsulePreflightError{Kind: CapsulePreflightPinMismatch, Requirement: "omnigent executable digest does not match the launch pin"}
	}
	versionOutput, err := p.preflightCommand(ctx, CapsulePreflightPinMismatch, "pinned omnigent version", []string{omnigentPath, "--version"})
	if err != nil {
		return err
	}
	matches := remotePinVersionPattern.FindStringSubmatch(strings.TrimSpace(string(versionOutput)))
	if matches == nil || matches[1] != capsule.ExecutablePin.PackageVersion || matches[2] == "" || !strings.HasPrefix(capsule.ExecutablePin.Commit, matches[2]) {
		return &CapsulePreflightError{Kind: CapsulePreflightPinMismatch, Requirement: "omnigent version or commit does not match the launch pin"}
	}

	paths := []struct {
		label      string
		path       string
		permission string
	}{
		{label: "workspace", path: cfg.WorkDir, permission: "-w"},
		{label: "capsule state root", path: capsule.State.MountPath, permission: "-w"},
		{label: "capsule run root", path: capsule.RunRoot, permission: "-w"},
		{label: "capsule catalog root", path: capsule.CatalogMountPath, permission: "-r"},
	}
	for _, target := range paths {
		if target.path == "" {
			continue
		}
		if _, err := p.preflightCommand(ctx, CapsulePreflightUnwritablePath, target.label, []string{"test", "-d", target.path}); err != nil {
			return err
		}
		if _, err := p.preflightCommand(ctx, CapsulePreflightUnwritablePath, target.label, []string{"test", target.permission, target.path}); err != nil {
			return err
		}
	}
	for _, ref := range refs {
		for _, args := range [][]string{{"test", "-f", ref.SSH.Path}, {"test", "-r", ref.SSH.Path}} {
			if _, err := p.preflightCommand(ctx, CapsulePreflightMissingProfileAuth, fmt.Sprintf("secret reference %q", ref.ID), args); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Provider) preflightCommand(ctx context.Context, failureKind CapsulePreflightFailureKind, requirement string, argv []string) ([]byte, error) {
	out, code, err := p.conn.Exec(ctx, "", argv)
	if err != nil {
		return nil, &CapsulePreflightError{
			Kind: CapsulePreflightTransport, Requirement: requirement,
			Err: errors.Join(runtime.ErrRuntimeUnavailable, err),
		}
	}
	if code != 0 {
		return nil, &CapsulePreflightError{Kind: failureKind, Requirement: requirement, Err: fmt.Errorf("remote check exited %d", code)}
	}
	return out, nil
}

func (p *Provider) lookupRemoteBinary(ctx context.Context, label, executable string) (string, error) {
	out, err := p.preflightCommand(ctx, CapsulePreflightMissingBinary, label, []string{
		"sh", "-c", `command -v -- "$1"`, "gc-preflight", executable,
	})
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", &CapsulePreflightError{Kind: CapsulePreflightMissingBinary, Requirement: label + " did not resolve to an absolute executable path"}
	}
	return path, nil
}

func supportedRemoteTmuxVersion(output string) bool {
	matches := remoteTmuxVersionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if matches == nil {
		return false
	}
	major, majorErr := strconv.Atoi(matches[1])
	minor, minorErr := strconv.Atoi(matches[2])
	return majorErr == nil && minorErr == nil && (major > 3 || major == 3 && minor >= 2)
}
