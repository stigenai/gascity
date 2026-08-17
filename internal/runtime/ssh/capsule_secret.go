package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const capsuleCredentialLauncherName = ".gc-omnigent-credentials-v1"

func rejectSSHCapsuleCredentialLiterals(cfg runtime.Config) error {
	if cfg.Capsule == nil {
		return nil
	}
	for key, value := range cfg.Env {
		if strings.TrimSpace(value) != "" && execenv.IsSensitiveKey(key) {
			return fmt.Errorf("capsule credential environment %s requires a typed SSH secret reference", key)
		}
	}
	return nil
}

func (p *Provider) projectCapsuleCredentials(ctx context.Context, logical, projected runtime.Config) (runtime.Config, error) {
	if projected.Capsule == nil || len(projected.SecretReferences) == 0 {
		return projected, nil
	}
	refs, err := runtime.SelectSecretReferences(runtime.SecretProviderSSH, projected.SecretReferences)
	if err != nil {
		return runtime.Config{}, err
	}
	refs = append([]runtime.SecretReference(nil), refs...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })

	for _, ref := range refs {
		if ref.MountPath == "" {
			continue
		}
		if logical.Capsule == nil || !sshPathStrictlyWithin(logical.Capsule.RunRoot, ref.MountPath) {
			return runtime.Config{}, fmt.Errorf("secret reference %q: SSH credential mount must be beneath capsule run root", ref.ID)
		}
		parent := filepath.Dir(ref.MountPath)
		relative, err := filepath.Rel(logical.Capsule.RunRoot, parent)
		if err != nil {
			return runtime.Config{}, fmt.Errorf("secret reference %q: resolve SSH credential mount parent", ref.ID)
		}
		if relative != "." {
			if err := p.ensureRemoteOwnedDir(ctx, logical.Capsule.RunRoot, filepath.ToSlash(relative)); err != nil {
				return runtime.Config{}, fmt.Errorf("prepare SSH credential destination for reference %q: %w", ref.ID, err)
			}
		}
	}

	launcher := renderCapsuleCredentialLauncher(refs)
	if err := p.stageCapsuleCredentialLauncher(ctx, projected.Capsule.RunRoot, []byte(launcher)); err != nil {
		return runtime.Config{}, err
	}
	clone := *projected.Capsule
	launcherPath := filepath.Join(projected.Capsule.RunRoot, capsuleCredentialLauncherName)
	clone.Command = append([]string{"sh", launcherPath, "--"}, projected.Capsule.Command...)
	projected.Capsule = &clone
	return projected, nil
}

func renderCapsuleCredentialLauncher(refs []runtime.SecretReference) string {
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -f\ngc_credential_failure() { printf '%s\\n' 'gc: SSH capsule credential projection failed' >&2; exit 78; }\n")
	script.WriteString(`gc_validate_credential_source() {
  gc_credential_path=$1
  [ -e "$gc_credential_path" ] && [ ! -L "$gc_credential_path" ] && { [ -f "$gc_credential_path" ] || [ -d "$gc_credential_path" ]; } && [ -r "$gc_credential_path" ] || gc_credential_failure
  gc_credential_uid=$(id -u 2>/dev/null) || gc_credential_failure
  if stat -c %u "$gc_credential_path" >/dev/null 2>&1; then
    gc_credential_owner=$(stat -c %u "$gc_credential_path" 2>/dev/null) || gc_credential_failure
    gc_credential_mode=$(stat -c %a "$gc_credential_path" 2>/dev/null) || gc_credential_failure
  elif stat -f %u "$gc_credential_path" >/dev/null 2>&1; then
    gc_credential_owner=$(stat -f %u "$gc_credential_path" 2>/dev/null) || gc_credential_failure
    gc_credential_mode=$(stat -f %Lp "$gc_credential_path" 2>/dev/null) || gc_credential_failure
  else
    gc_credential_failure
  fi
  [ "$gc_credential_owner" = "$gc_credential_uid" ] || gc_credential_failure
  case $gc_credential_mode in ''|*[!0-7]*) gc_credential_failure ;; *00) ;; *) gc_credential_failure ;; esac
  unset gc_credential_path gc_credential_uid gc_credential_owner gc_credential_mode
}
`)
	script.WriteString("[ \"$#\" -gt 1 ] && [ \"$1\" = -- ] || gc_credential_failure\nshift\n")
	for _, ref := range refs {
		source := shellquote.Quote(ref.SSH.Path)
		if ref.Environment != "" {
			script.WriteString("gc_credential_source=" + source + "\n")
			script.WriteString("gc_validate_credential_source \"$gc_credential_source\"\n")
			script.WriteString("if [ -d \"$gc_credential_source\" ]; then gc_credential_value=$gc_credential_source; else gc_credential_value=$(cat \"$gc_credential_source\" 2>/dev/null) || gc_credential_failure; fi\n")
			script.WriteString(ref.Environment + "=$gc_credential_value\nexport " + ref.Environment + "\n")
			script.WriteString("unset gc_credential_source gc_credential_value\n")
			continue
		}
		destination := shellquote.Quote(ref.MountPath)
		script.WriteString("gc_credential_source=" + source + "\ngc_credential_destination=" + destination + "\n")
		script.WriteString("gc_validate_credential_source \"$gc_credential_source\"\n")
		script.WriteString("if [ -L \"$gc_credential_destination\" ]; then [ \"$(readlink \"$gc_credential_destination\" 2>/dev/null)\" = \"$gc_credential_source\" ] || gc_credential_failure; elif [ -e \"$gc_credential_destination\" ]; then gc_credential_failure; else ln -s \"$gc_credential_source\" \"$gc_credential_destination\" 2>/dev/null || gc_credential_failure; fi\n")
		script.WriteString("unset gc_credential_source gc_credential_destination\n")
	}
	script.WriteString("exec \"$@\"\n")
	return script.String()
}

func (p *Provider) stageCapsuleCredentialLauncher(ctx context.Context, root string, payload []byte) error {
	digest := sha256.Sum256(payload)
	input := runtime.CapsuleInput{
		RelativePath: capsuleCredentialLauncherName,
		SHA256:       "sha256:" + hex.EncodeToString(digest[:]),
		Mode:         0o700,
	}
	argv := []string{
		"sh", "-c", remoteAtomicStageScript, "gc-capsule-secret-stage-v1",
		root, input.RelativePath, input.SHA256, "0700",
	}
	_, code, runErr := p.conn.run.run(ctx, p.conn.ep, argv, payload)
	if runErr == nil && code == 0 {
		return nil
	}
	verified, verifyErr := p.verifyRemoteInput(ctx, root, input)
	if verifyErr == nil && verified {
		return nil
	}
	if runErr != nil {
		return &CapsuleStageError{
			Kind: CapsuleStageTransport, Destination: "credential launcher",
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
	return &CapsuleStageError{Kind: kind, Destination: "credential launcher", Err: fmt.Errorf("remote stage exited %d", code)}
}

func sshPathStrictlyWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

const remoteCapsuleCredentialCheckScript = `
set -f
path=$2
[ -e "$path" ] || exit 44
[ ! -L "$path" ] && { [ -f "$path" ] || [ -d "$path" ]; } && [ -r "$path" ] || exit 73
uid=$(id -u) || exit 76
if stat -c %u "$path" >/dev/null 2>&1; then
  owner=$(stat -c %u "$path") || exit 76
  mode=$(stat -c %a "$path") || exit 76
elif stat -f %u "$path" >/dev/null 2>&1; then
  owner=$(stat -f %u "$path") || exit 76
  mode=$(stat -f %Lp "$path") || exit 76
else
  exit 76
fi
[ "$owner" = "$uid" ] || exit 74
case $mode in ''|*[!0-7]*) exit 76 ;; *00) ;; *) exit 75 ;; esac
`
