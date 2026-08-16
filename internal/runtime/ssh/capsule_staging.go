package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/runtime"
)

// CapsuleStageFailureKind is the stable class of an SSH staging failure.
type CapsuleStageFailureKind string

const (
	// CapsuleStageInvalidSource rejects absent, symlinked, or non-regular input.
	CapsuleStageInvalidSource CapsuleStageFailureKind = "invalid-source"
	// CapsuleStageChecksumMismatch rejects controller or remote byte drift.
	CapsuleStageChecksumMismatch CapsuleStageFailureKind = "checksum-mismatch"
	// CapsuleStageContainment rejects a remote symlink or ownership escape.
	CapsuleStageContainment CapsuleStageFailureKind = "containment"
	// CapsuleStageTransport reports an inconclusive SSH transport operation.
	CapsuleStageTransport CapsuleStageFailureKind = "transport"
	// CapsuleStageRemoteWrite reports another remote atomic-write failure.
	CapsuleStageRemoteWrite CapsuleStageFailureKind = "remote-write"
)

// CapsuleStageError identifies one non-secret staged destination and failure
// class. It never includes controller source or remote credential paths.
type CapsuleStageError struct {
	Kind        CapsuleStageFailureKind
	Destination string
	Err         error
}

func (e *CapsuleStageError) Error() string {
	message := "ssh capsule staging " + string(e.Kind)
	if e.Destination != "" {
		message += ": " + e.Destination
	}
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	return message
}

// Unwrap preserves transport sentinels for controller retry policy.
func (e *CapsuleStageError) Unwrap() error { return e.Err }

func (p *Provider) stageCapsuleInputs(ctx context.Context, cfg runtime.Config) error {
	if cfg.Capsule == nil {
		return nil
	}
	if len(cfg.Capsule.CatalogInputs) == 0 {
		return &CapsuleStageError{Kind: CapsuleStageInvalidSource, Destination: "catalog inputs are required"}
	}
	return p.stageRemoteInputs(ctx, cfg.Capsule.CatalogMountPath, cfg.Capsule.CatalogInputs)
}

func (p *Provider) stageRemoteInputs(ctx context.Context, root string, inputs []runtime.CapsuleInput) error {
	for _, input := range inputs {
		payload, err := readStagedInput(input)
		if err != nil {
			return err
		}
		argv := []string{
			"sh", "-c", remoteAtomicStageScript, "gc-capsule-stage-v1",
			root, filepath.ToSlash(input.RelativePath), input.SHA256, fmt.Sprintf("%04o", input.Mode),
		}
		_, code, runErr := p.conn.run.run(ctx, p.conn.ep, argv, payload)
		if runErr == nil && code == 0 {
			continue
		}
		// SSH can lose the response after the rename committed. Reconnect and
		// accept only an exact digest/mode/owner verification.
		verified, verifyErr := p.verifyRemoteInput(ctx, root, input)
		if verifyErr == nil && verified {
			continue
		}
		if runErr != nil {
			return &CapsuleStageError{
				Kind: CapsuleStageTransport, Destination: input.RelativePath,
				Err: errors.Join(runtime.ErrRuntimeUnavailable, runErr),
			}
		}
		kind := CapsuleStageRemoteWrite
		switch code {
		case 73:
			kind = CapsuleStageContainment
		case 74:
			kind = CapsuleStageChecksumMismatch
		}
		return &CapsuleStageError{Kind: kind, Destination: input.RelativePath, Err: fmt.Errorf("remote stage exited %d", code)}
	}
	return nil
}

func readStagedInput(input runtime.CapsuleInput) ([]byte, error) {
	info, err := os.Lstat(input.SourcePath)
	if err != nil {
		return nil, &CapsuleStageError{Kind: CapsuleStageInvalidSource, Destination: input.RelativePath, Err: err}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, &CapsuleStageError{Kind: CapsuleStageInvalidSource, Destination: input.RelativePath, Err: errors.New("source must be a regular non-symlink file")}
	}
	payload, err := os.ReadFile(input.SourcePath)
	if err != nil {
		return nil, &CapsuleStageError{Kind: CapsuleStageInvalidSource, Destination: input.RelativePath, Err: err}
	}
	digest := sha256.Sum256(payload)
	if "sha256:"+hex.EncodeToString(digest[:]) != input.SHA256 {
		return nil, &CapsuleStageError{Kind: CapsuleStageChecksumMismatch, Destination: input.RelativePath, Err: errors.New("controller input digest does not match launch plan")}
	}
	return payload, nil
}

func (p *Provider) verifyRemoteInput(ctx context.Context, root string, input runtime.CapsuleInput) (bool, error) {
	argv := []string{
		"sh", "-c", remoteStageVerifyScript, "gc-capsule-verify-v1",
		root, filepath.ToSlash(input.RelativePath), input.SHA256, fmt.Sprintf("%04o", input.Mode),
	}
	_, code, err := p.conn.Exec(ctx, "", argv)
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

func capsuleInputFromCopyEntry(src, relativePath string) (runtime.CapsuleInput, error) {
	absSource, err := filepath.Abs(src)
	if err != nil {
		return runtime.CapsuleInput{}, err
	}
	payload, err := os.ReadFile(absSource)
	if err != nil {
		return runtime.CapsuleInput{}, err
	}
	digest := sha256.Sum256(payload)
	return runtime.CapsuleInput{
		SourcePath: absSource, RelativePath: relativePath,
		SHA256: "sha256:" + hex.EncodeToString(digest[:]), Mode: 0o644,
	}, nil
}

const remoteStageCommonScript = `
set -f
root=$1
rel=$2
expected=${3#sha256:}
mode=$4
if command -v sha256sum >/dev/null 2>&1; then
  hash_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  hash_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  exit 75
fi
if stat -c %a "$root" >/dev/null 2>&1; then
  file_mode() { stat -c %a "$1"; }
  file_owner() { stat -c %u "$1"; }
elif stat -f %Lp "$root" >/dev/null 2>&1; then
  file_mode() { stat -f %Lp "$1"; }
  file_owner() { stat -f %u "$1"; }
else
  exit 75
fi
uid=$(id -u) || exit 75
[ -d "$root" ] && [ ! -L "$root" ] && [ "$(file_owner "$root")" = "$uid" ] || exit 73
case $rel in ''|/*|..|../*|*/../*|*/..) exit 73 ;; esac
current=$root
parent=${rel%/*}
if [ "$parent" != "$rel" ]; then
  old_ifs=$IFS
  IFS=/
  for component in $parent; do
    [ -n "$component" ] && [ "$component" != . ] && [ "$component" != .. ] || exit 73
    current=$current/$component
    if [ -e "$current" ] || [ -L "$current" ]; then
      [ -d "$current" ] && [ ! -L "$current" ] && [ "$(file_owner "$current")" = "$uid" ] || exit 73
    else
      mkdir -m 0755 "$current" || exit 76
    fi
  done
  IFS=$old_ifs
fi
target=$root/$rel
if [ -e "$target" ] || [ -L "$target" ]; then
  [ -f "$target" ] && [ ! -L "$target" ] && [ "$(file_owner "$target")" = "$uid" ] || exit 73
fi
`

const remoteStageVerifyScript = remoteStageCommonScript + `
[ -f "$target" ] && [ "$(hash_file "$target")" = "$expected" ] && [ "$(file_mode "$target")" = "${mode#0}" ]
`

const remoteAtomicStageScript = remoteStageCommonScript + `
if [ -f "$target" ] && [ "$(hash_file "$target")" = "$expected" ] && [ "$(file_mode "$target")" = "${mode#0}" ]; then
  exit 0
fi
umask 077
tmp=$target.gc-stage.$$
trap 'rm -f "$tmp"' EXIT HUP INT TERM
cat >"$tmp" || exit 76
chmod "$mode" "$tmp" || exit 76
[ "$(hash_file "$tmp")" = "$expected" ] || exit 74
[ "$(file_owner "$tmp")" = "$uid" ] || exit 73
mv -f "$tmp" "$target" || exit 76
trap - EXIT HUP INT TERM
[ "$(hash_file "$target")" = "$expected" ] && [ "$(file_mode "$target")" = "${mode#0}" ] && [ "$(file_owner "$target")" = "$uid" ] || exit 74
`
