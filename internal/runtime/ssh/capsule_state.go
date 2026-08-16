package ssh

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	defaultCapsuleStateRoot   = "/var/lib/gascity/omnigent"
	capsuleStateIdentityFile  = ".gc-capsule-identity-v1"
	capsuleStateProtocolLabel = "gc-ssh-capsule-state-v1"
)

var _ runtime.CapsuleStateRuntime = (*Provider)(nil)

type capsuleStateAttachment struct {
	name string
	uid  string
}

// EnsureCapsuleState creates or reopens one deterministic, owner-only remote
// directory. The identity record is allocation metadata, not process status;
// liveness and attachment are always queried from remote tmux ground truth.
func (p *Provider) EnsureCapsuleState(ctx context.Context, key runtime.CapsuleKey) (runtime.CapsuleStateReference, bool, error) {
	if err := key.Validate(); err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	root, err := p.validCapsuleStateRoot()
	if err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	args := append([]string{"sh", "-c", remoteEnsureCapsuleStateScript, capsuleStateProtocolLabel}, capsuleStateArguments(root, key)...)
	out, code, runErr := p.conn.Exec(ctx, "", args)
	if runErr != nil {
		// The mkdir and identity rename may have committed before the SSH reply
		// was lost. Reconnect and accept only an exact ownership match.
		if ref, ok, openErr := p.OpenCapsuleState(ctx, key); openErr == nil && ok {
			return ref, false, nil
		}
		return runtime.CapsuleStateReference{}, false, fmt.Errorf("ensure SSH capsule state: %w", errors.Join(runtime.ErrRuntimeUnavailable, runErr))
	}
	if code != 0 {
		return runtime.CapsuleStateReference{}, false, capsuleStateCommandError("ensure", key.ResourceStem(), code)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || (fields[1] != "0" && fields[1] != "1") {
		return runtime.CapsuleStateReference{}, false, fmt.Errorf("%w: malformed SSH capsule state ensure response", runtime.ErrCapsuleStateConflict)
	}
	ref, err := p.sshCapsuleStateReference(root, key, key.ResourceStem(), fields[0])
	return ref, fields[1] == "1", err
}

// OpenCapsuleState returns one exact existing remote allocation without
// creating or changing it.
func (p *Provider) OpenCapsuleState(ctx context.Context, key runtime.CapsuleKey) (runtime.CapsuleStateReference, bool, error) {
	if err := key.Validate(); err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	root, err := p.validCapsuleStateRoot()
	if err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	args := append([]string{"sh", "-c", remoteOpenCapsuleStateScript, capsuleStateProtocolLabel}, capsuleStateArguments(root, key)...)
	out, code, runErr := p.conn.Exec(ctx, "", args)
	if runErr != nil {
		return runtime.CapsuleStateReference{}, false, fmt.Errorf("open SSH capsule state: %w", errors.Join(runtime.ErrRuntimeUnavailable, runErr))
	}
	if code == 44 {
		return runtime.CapsuleStateReference{}, false, nil
	}
	if code != 0 {
		return runtime.CapsuleStateReference{}, false, capsuleStateCommandError("open", key.ResourceStem(), code)
	}
	uid := strings.TrimSpace(string(out))
	ref, err := p.sshCapsuleStateReference(root, key, key.ResourceStem(), uid)
	return ref, err == nil, err
}

// ListCapsuleStates returns a deterministic inventory derived solely from
// exact, owner-verified remote allocation metadata.
func (p *Provider) ListCapsuleStates(ctx context.Context) ([]runtime.CapsuleStateReference, error) {
	root, err := p.validCapsuleStateRoot()
	if err != nil {
		return nil, err
	}
	out, code, runErr := p.conn.Exec(ctx, "", []string{"sh", "-c", remoteListCapsuleStateScript, capsuleStateProtocolLabel, root})
	if runErr != nil {
		return nil, fmt.Errorf("list SSH capsule state: %w", errors.Join(runtime.ErrRuntimeUnavailable, runErr))
	}
	if code != 0 {
		return nil, capsuleStateCommandError("list", "", code)
	}
	var refs []runtime.CapsuleStateReference
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			return nil, fmt.Errorf("%w: malformed SSH capsule inventory line %d", runtime.ErrCapsuleStateConflict, lineNumber+1)
		}
		version, parseErr := strconv.Atoi(fields[2])
		cityBytes, cityErr := hex.DecodeString(fields[5])
		sessionBytes, sessionErr := hex.DecodeString(fields[6])
		if parseErr != nil || cityErr != nil || sessionErr != nil {
			return nil, fmt.Errorf("%w: malformed SSH capsule inventory identity", runtime.ErrCapsuleStateConflict)
		}
		key, keyErr := runtime.NewCapsuleKey(string(cityBytes), string(sessionBytes))
		if keyErr != nil || version != key.Version || fields[0] != key.ResourceStem() || fields[3] != key.Digest || fields[4] != key.Token {
			return nil, fmt.Errorf("%w: SSH capsule inventory identity does not match its derivation", runtime.ErrCapsuleStateConflict)
		}
		ref, refErr := p.sshCapsuleStateReference(root, key, fields[0], fields[1])
		if refErr != nil {
			return nil, refErr
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ResourceID < refs[j].ResourceID })
	return refs, nil
}

// PurgeCapsuleState atomically renames and deletes only the exact allocation.
// Missing state is idempotent. A tmux session carrying the allocation UID makes
// the purge fail closed.
func (p *Provider) PurgeCapsuleState(ctx context.Context, key runtime.CapsuleKey) error {
	ref, ok, err := p.OpenCapsuleState(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		return p.finalizeAbsentCapsulePurge(ctx, key)
	}
	root, err := p.validCapsuleStateRoot()
	if err != nil {
		return err
	}
	args := append([]string{"sh", "-c", remotePurgeCapsuleStateScript, capsuleStateProtocolLabel}, capsuleStateArguments(root, key)...)
	args = append(args, ref.ResourceUID)
	_, code, runErr := p.conn.Exec(ctx, "", args)
	if runErr != nil {
		if _, stillPresent, openErr := p.OpenCapsuleState(ctx, key); openErr == nil && !stillPresent && p.finalizeAbsentCapsulePurge(ctx, key) == nil {
			return nil
		}
		return fmt.Errorf("purge SSH capsule state: %w", errors.Join(runtime.ErrRuntimeUnavailable, runErr))
	}
	if code == 44 {
		return p.finalizeAbsentCapsulePurge(ctx, key)
	}
	if code != 0 {
		return capsuleStateCommandError("purge", key.ResourceStem(), code)
	}
	return nil
}

func (p *Provider) finalizeAbsentCapsulePurge(ctx context.Context, key runtime.CapsuleKey) error {
	root, err := p.validCapsuleStateRoot()
	if err != nil {
		return err
	}
	args := append([]string{"sh", "-c", remoteFinalizeCapsulePurgeScript, capsuleStateProtocolLabel}, capsuleStateArguments(root, key)...)
	_, code, runErr := p.conn.Exec(ctx, "", args)
	if runErr != nil {
		return fmt.Errorf("finalize SSH capsule purge: %w", errors.Join(runtime.ErrRuntimeUnavailable, runErr))
	}
	if code != 0 {
		return capsuleStateCommandError("finalize purge", key.ResourceStem(), code)
	}
	return nil
}

// AttachCapsuleState verifies the exact allocation and rejects a tmux session
// other than placeName already carrying it. The actual attachment marker is a
// tmux user option written immediately after session creation, so it disappears
// with tmux and can never become a stale status file.
func (p *Provider) AttachCapsuleState(ctx context.Context, placeName string, ref runtime.CapsuleStateReference) error {
	opened, ok, err := p.OpenCapsuleState(ctx, ref.Key)
	if err != nil {
		return err
	}
	if !ok || opened != ref {
		return fmt.Errorf("%w: SSH capsule state reference is missing or stale", runtime.ErrCapsuleStateConflict)
	}
	attachments, err := p.listCapsuleStateAttachments(ctx)
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		if attachment.uid == ref.ResourceUID && attachment.name != placeName {
			return fmt.Errorf("%w: SSH capsule state is attached to another Place", runtime.ErrCapsuleStateConflict)
		}
	}
	return nil
}

// DetachCapsuleState succeeds only after the named remote tmux session is gone
// or no longer carries capsule state. Teardown owns the physical detachment.
func (p *Provider) DetachCapsuleState(ctx context.Context, placeName string) error {
	attachments, err := p.listCapsuleStateAttachments(ctx)
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		if attachment.name == placeName && attachment.uid != "" {
			return fmt.Errorf("%w: SSH capsule Place must be torn down before detach completes", runtime.ErrCapsuleStateConflict)
		}
	}
	return nil
}

func (p *Provider) markCapsuleStateAttached(ctx context.Context, placeName string, ref runtime.CapsuleStateReference) error {
	if err := p.AttachCapsuleState(ctx, placeName, ref); err != nil {
		return err
	}
	_, code, err := p.tmux(ctx, placeName, "set-option", "-t", placeName, capsuleStateTMUXOption, ref.ResourceUID)
	if err != nil {
		return fmt.Errorf("mark SSH capsule state attachment: %w", errors.Join(runtime.ErrRuntimeUnavailable, err))
	}
	if code != 0 {
		return fmt.Errorf("%w: mark SSH capsule state attachment exited %d", runtime.ErrCapsuleStateConflict, code)
	}
	return nil
}

func (p *Provider) requireCapsuleStateAttachment(ctx context.Context, placeName string, ref runtime.CapsuleStateReference) error {
	attachments, err := p.listCapsuleStateAttachments(ctx)
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		if attachment.name == placeName {
			if attachment.uid == ref.ResourceUID {
				return nil
			}
			return fmt.Errorf("%w: SSH capsule tmux session carries different durable state", runtime.ErrCapsuleStateConflict)
		}
	}
	return fmt.Errorf("%w: SSH capsule tmux session has no durable-state attachment", runtime.ErrCapsuleStateConflict)
}

func (p *Provider) listCapsuleStateAttachments(ctx context.Context) ([]capsuleStateAttachment, error) {
	out, code, err := p.tmux(ctx, "", "list-sessions", "-F", "#{session_name}\t#{@gc_capsule_state_uid}")
	if err != nil {
		return nil, fmt.Errorf("list SSH capsule state attachments: %w", errors.Join(runtime.ErrRuntimeUnavailable, err))
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var attachments []capsuleStateAttachment
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || fields[0] == "" || strings.ContainsAny(fields[1], "\r\n") {
			return nil, fmt.Errorf("%w: malformed SSH tmux capsule attachment inventory", runtime.ErrCapsuleStateConflict)
		}
		attachments = append(attachments, capsuleStateAttachment{name: fields[0], uid: fields[1]})
	}
	return attachments, nil
}

func (p *Provider) validCapsuleStateRoot() (string, error) {
	root := filepath.Clean(strings.TrimSpace(p.capsuleStateRoot))
	if root == "." || root == string(filepath.Separator) || !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: SSH capsule state root must be a clean absolute non-root path", runtime.ErrCapsuleStateConflict)
	}
	return root, nil
}

func capsuleStateArguments(root string, key runtime.CapsuleKey) []string {
	return []string{
		root, key.ResourceStem(), strconv.Itoa(key.Version), key.Digest, key.Token,
		hex.EncodeToString([]byte(key.CityScope)), hex.EncodeToString([]byte(key.SessionID)), capsuleStateIdentityFile,
	}
}

func (p *Provider) sshCapsuleStateReference(root string, key runtime.CapsuleKey, resourceID, resourceUID string) (runtime.CapsuleStateReference, error) {
	if resourceID != key.ResourceStem() || strings.TrimSpace(resourceUID) == "" || strings.ContainsAny(resourceUID, "\t\r\n") {
		return runtime.CapsuleStateReference{}, fmt.Errorf("%w: SSH capsule allocation identity is incomplete", runtime.ErrCapsuleStateConflict)
	}
	return runtime.CapsuleStateReference{
		Key: key, Provider: string(runtime.SecretProviderSSH), ResourceID: resourceID,
		ResourceUID: resourceUID, MountPath: root,
	}, nil
}

func capsuleStateCommandError(operation, resource string, code int) error {
	switch code {
	case 73, 76:
		return fmt.Errorf("%w: SSH capsule state %s rejected for %q", runtime.ErrCapsuleStateConflict, operation, resource)
	case 75:
		return fmt.Errorf("SSH capsule state %s is unsupported by the remote host", operation)
	default:
		return fmt.Errorf("SSH capsule state %s for %q exited %d", operation, resource, code)
	}
}

const remoteCapsuleStateCommonScript = `
set -f
root=$1
resource=$2
version=$3
digest=$4
token=$5
city_hex=$6
session_hex=$7
identity_name=$8
case $resource in ''|*[!a-z0-9-]*) exit 73 ;; esac
case $identity_name in ''|/*|*/*|.|..) exit 73 ;; esac
if stat -c %a "$root" >/dev/null 2>&1; then
  file_mode() { stat -c %a "$1"; }
  file_owner() { stat -c %u "$1"; }
  file_identity() { stat -c '%d:%i' "$1"; }
elif stat -f %Lp "$root" >/dev/null 2>&1; then
  file_mode() { stat -f %Lp "$1"; }
  file_owner() { stat -f %u "$1"; }
  file_identity() { stat -f '%d:%i' "$1"; }
else
  exit 75
fi
uid=$(id -u) || exit 75
[ -d "$root" ] && [ ! -L "$root" ] && [ "$(file_owner "$root")" = "$uid" ] || exit 73
root_mode=$(file_mode "$root") || exit 73
[ $((0$root_mode & 022)) -eq 0 ] || exit 73
target=$root/$resource
identity=$target/$identity_name
expected=$(printf '%s\t%s\t%s\t%s\t%s' "$version" "$digest" "$token" "$city_hex" "$session_hex")
validate_allocation() {
  [ -d "$target" ] && [ ! -L "$target" ] && [ "$(file_owner "$target")" = "$uid" ] || return 1
  target_mode=$(file_mode "$target") || return 1
  [ $((0$target_mode & 077)) -eq 0 ] || return 1
  [ -f "$identity" ] && [ ! -L "$identity" ] && [ "$(file_owner "$identity")" = "$uid" ] || return 1
  [ "$(file_mode "$identity")" = 600 ] || return 1
  [ "$(cat "$identity")" = "$expected" ] || return 1
  return 0
}
`

const remoteEnsureCapsuleStateScript = remoteCapsuleStateCommonScript + `
created=0
if [ ! -e "$target" ] && [ ! -L "$target" ]; then
  if mkdir -m 0700 "$target" 2>/dev/null; then
    created=1
    umask 077
    tmp=$target/.gc-identity.$$
    trap 'rm -f "$tmp"' EXIT HUP INT TERM
    printf '%s\n' "$expected" >"$tmp" || exit 76
    chmod 0600 "$tmp" || exit 76
    mv -f "$tmp" "$identity" || exit 76
    trap - EXIT HUP INT TERM
  fi
fi
if ! validate_allocation; then
  attempts=0
  while [ "$attempts" -lt 20 ] && [ ! -e "$identity" ] && [ ! -L "$identity" ]; do
    attempts=$((attempts + 1))
    sleep 0.05
  done
  validate_allocation || exit 73
fi
printf '%s\t%s\n' "$(file_identity "$target")" "$created"
`

const remoteOpenCapsuleStateScript = remoteCapsuleStateCommonScript + `
if [ ! -e "$target" ] && [ ! -L "$target" ]; then exit 44; fi
validate_allocation || exit 73
printf '%s\n' "$(file_identity "$target")"
`

const remoteListCapsuleStateScript = `
set -f
root=$1
if stat -c %a "$root" >/dev/null 2>&1; then
  file_mode() { stat -c %a "$1"; }
  file_owner() { stat -c %u "$1"; }
  file_identity() { stat -c '%d:%i' "$1"; }
elif stat -f %Lp "$root" >/dev/null 2>&1; then
  file_mode() { stat -f %Lp "$1"; }
  file_owner() { stat -f %u "$1"; }
  file_identity() { stat -f '%d:%i' "$1"; }
else
  exit 75
fi
uid=$(id -u) || exit 75
[ -d "$root" ] && [ ! -L "$root" ] && [ "$(file_owner "$root")" = "$uid" ] || exit 73
set +f
for target in "$root"/gco-*; do
  [ -e "$target" ] || [ -L "$target" ] || continue
  [ -d "$target" ] && [ ! -L "$target" ] && [ "$(file_owner "$target")" = "$uid" ] || exit 73
  target_mode=$(file_mode "$target") || exit 73
  [ $((0$target_mode & 077)) -eq 0 ] || exit 73
  resource=${target##*/}
  identity=$target/.gc-capsule-identity-v1
  [ -f "$identity" ] && [ ! -L "$identity" ] && [ "$(file_owner "$identity")" = "$uid" ] || exit 73
  [ "$(file_mode "$identity")" = 600 ] || exit 73
  fields=$(cat "$identity") || exit 73
  old_ifs=$IFS
  tab=$(printf '\t')
  IFS=$tab
  set -- $fields
  IFS=$old_ifs
  [ "$#" -eq 5 ] || exit 73
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$resource" "$(file_identity "$target")" "$1" "$2" "$3" "$4" "$5"
done
`

const remotePurgeCapsuleStateScript = remoteCapsuleStateCommonScript + `
expected_uid=$9
if [ ! -e "$target" ] && [ ! -L "$target" ]; then exit 44; fi
validate_allocation || exit 73
[ "$(file_identity "$target")" = "$expected_uid" ] || exit 73
if command -v tmux >/dev/null 2>&1; then
  attached=$(tmux list-sessions -F '#{@gc_capsule_state_uid}' 2>/dev/null) || attached=
  for attached_uid in $attached; do
    [ "$attached_uid" != "$expected_uid" ] || exit 76
  done
fi
tombstone=$root/.gc-purge-$resource-$$
[ ! -e "$tombstone" ] && [ ! -L "$tombstone" ] || exit 73
mv "$target" "$tombstone" || exit 76
[ -d "$tombstone" ] && [ ! -L "$tombstone" ] && [ "$(file_identity "$tombstone")" = "$expected_uid" ] || exit 73
rm -rf -- "$tombstone" || exit 76
[ ! -e "$target" ] && [ ! -L "$target" ] || exit 76
`

const remoteFinalizeCapsulePurgeScript = remoteCapsuleStateCommonScript + `
[ ! -e "$target" ] && [ ! -L "$target" ] || exit 73
set +f
found=0
for tombstone in "$root"/.gc-purge-$resource-*; do
  [ -e "$tombstone" ] || [ -L "$tombstone" ] || continue
  found=1
  target=$tombstone
  identity=$target/$identity_name
  validate_allocation || exit 73
  rm -rf -- "$target" || exit 76
done
[ ! -e "$root/$resource" ] && [ ! -L "$root/$resource" ] || exit 73
exit 0
`
