package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

const capsuleStateTMUXOption = "@gc_capsule_state_uid"

func (p *Provider) prepareCapsuleLaunch(ctx context.Context, name string, cfg runtime.Config) (runtime.Config, error) {
	if err := p.preflightCapsule(ctx, cfg); err != nil {
		return runtime.Config{}, err
	}
	if cfg.Capsule == nil {
		return cfg, nil
	}
	if err := p.rememberCapsuleCleanupRoots(cfg.Capsule); err != nil {
		return runtime.Config{}, err
	}
	projected, err := p.projectCapsulePaths(ctx, name, cfg)
	if err != nil {
		return runtime.Config{}, err
	}
	statePath, runRoot, _, catalogRoot := p.capsulePhysicalPaths(cfg.Capsule)
	if err := p.ensureRemoteOwnedDir(ctx, cfg.Capsule.RunRoot, filepath.Base(runRoot)); err != nil {
		return runtime.Config{}, fmt.Errorf("prepare SSH capsule run directory: %w", err)
	}
	catalogRelative, err := filepath.Rel(cfg.Capsule.CatalogMountPath, catalogRoot)
	if err != nil {
		return runtime.Config{}, fmt.Errorf("derive SSH capsule catalog directory: %w", err)
	}
	if err := p.ensureRemoteOwnedDir(ctx, cfg.Capsule.CatalogMountPath, catalogRelative); err != nil {
		return runtime.Config{}, fmt.Errorf("prepare SSH capsule catalog directory: %w", err)
	}
	if err := p.stageCapsuleInputs(ctx, projected); err != nil {
		return runtime.Config{}, err
	}
	projected, err = p.projectCapsuleCredentials(ctx, cfg, projected)
	if err != nil {
		return runtime.Config{}, err
	}
	// OpenCapsuleState already verified statePath. Keep this explicit so future
	// path projection cannot silently point the command at the shared base root.
	if statePath == cfg.Capsule.State.MountPath {
		return runtime.Config{}, fmt.Errorf("%w: SSH capsule state was not session-isolated", runtime.ErrCapsuleStateConflict)
	}
	return projected, nil
}

func (p *Provider) projectCapsulePaths(ctx context.Context, name string, cfg runtime.Config) (runtime.Config, error) {
	capsule := cfg.Capsule
	if capsule == nil {
		return cfg, nil
	}
	root, err := p.validCapsuleStateRoot()
	if err != nil {
		return runtime.Config{}, err
	}
	if capsule.State.Provider != string(runtime.SecretProviderSSH) || capsule.State.MountPath != root || capsule.State.ResourceID != capsule.Key.ResourceStem() {
		return runtime.Config{}, fmt.Errorf("%w: SSH capsule state reference does not match this provider", runtime.ErrCapsuleStateConflict)
	}
	opened, ok, err := p.OpenCapsuleState(ctx, capsule.Key)
	if err != nil {
		return runtime.Config{}, err
	}
	if !ok || opened != capsule.State {
		return runtime.Config{}, fmt.Errorf("%w: SSH capsule state is missing or stale", runtime.ErrCapsuleStateConflict)
	}
	if err := p.AttachCapsuleState(ctx, name, capsule.State); err != nil {
		return runtime.Config{}, err
	}
	statePath, runRoot, socketPath, catalogRoot := p.capsulePhysicalPaths(capsule)
	clone := *capsule
	clone.Command = replaceCapsuleCommandPaths(capsule.Command, capsule, statePath, socketPath, catalogRoot)
	clone.CatalogInputs = append([]runtime.CapsuleInput(nil), capsule.CatalogInputs...)
	clone.RunRoot = runRoot
	clone.SocketPath = socketPath
	clone.CatalogMountPath = catalogRoot
	projected := cfg
	projected.Capsule = &clone
	if err := clone.Validate(); err != nil {
		return runtime.Config{}, fmt.Errorf("project SSH capsule paths: %w", err)
	}
	return projected, nil
}

func (p *Provider) capsulePhysicalPaths(capsule *runtime.CapsuleLaunchConfig) (statePath, runRoot, socketPath, catalogRoot string) {
	resource := capsule.Key.ResourceStem()
	statePath = filepath.Join(filepath.Clean(p.capsuleStateRoot), resource)
	runRoot = filepath.Join(capsule.RunRoot, resource)
	socketPath = filepath.Join(runRoot, filepath.Base(capsule.SocketPath))
	digest := sha256.Sum256([]byte(capsule.CatalogResourceID))
	catalogRoot = filepath.Join(capsule.CatalogMountPath, resource, "generation-"+hex.EncodeToString(digest[:10]))
	return statePath, runRoot, socketPath, catalogRoot
}

func replaceCapsuleCommandPaths(command []string, capsule *runtime.CapsuleLaunchConfig, statePath, socketPath, catalogRoot string) []string {
	projected := append([]string(nil), command...)
	for i, arg := range projected {
		switch arg {
		case capsule.State.MountPath:
			projected[i] = statePath
		case capsule.SocketPath:
			projected[i] = socketPath
		default:
			if rel, err := filepath.Rel(capsule.CatalogMountPath, arg); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				projected[i] = filepath.Join(catalogRoot, rel)
			}
		}
	}
	return projected
}

func (p *Provider) ensureRemoteOwnedDir(ctx context.Context, root, relative string) error {
	_, code, err := p.conn.Exec(ctx, "", []string{"sh", "-c", remoteEnsureOwnedDirScript, "gc-ssh-capsule-dir-v1", root, filepath.ToSlash(relative)})
	if err != nil {
		return errors.Join(runtime.ErrRuntimeUnavailable, err)
	}
	if code != 0 {
		if code == 73 {
			return fmt.Errorf("%w: remote directory containment rejected", runtime.ErrCapsuleStateConflict)
		}
		return fmt.Errorf("remote directory preparation exited %d", code)
	}
	return nil
}

const remoteEnsureOwnedDirScript = `
set -f
root=$1
rel=$2
case $rel in ''|/*|..|../*|*/../*|*/..) exit 73 ;; esac
if stat -c %u "$root" >/dev/null 2>&1; then
  file_owner() { stat -c %u "$1"; }
elif stat -f %u "$root" >/dev/null 2>&1; then
  file_owner() { stat -f %u "$1"; }
else
  exit 75
fi
uid=$(id -u) || exit 75
[ -d "$root" ] && [ ! -L "$root" ] && [ "$(file_owner "$root")" = "$uid" ] || exit 73
current=$root
old_ifs=$IFS
IFS=/
for component in $rel; do
  [ -n "$component" ] && [ "$component" != . ] && [ "$component" != .. ] || exit 73
  current=$current/$component
  if [ -e "$current" ] || [ -L "$current" ]; then
    [ -d "$current" ] && [ ! -L "$current" ] && [ "$(file_owner "$current")" = "$uid" ] || exit 73
  else
    mkdir -m 0700 "$current" || exit 76
  fi
done
IFS=$old_ifs
`
