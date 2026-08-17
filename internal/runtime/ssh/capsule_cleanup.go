package ssh

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	defaultCapsuleRunRoot     = "/run/gascity/omnigent"
	defaultCapsuleCatalogRoot = "/etc/gascity/omnigent"
)

func (p *Provider) rememberCapsuleCleanupRoots(capsule *runtime.CapsuleLaunchConfig) error {
	if capsule == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.capsuleRunRoot != "" && p.capsuleRunRoot != capsule.RunRoot {
		return fmt.Errorf("%w: SSH capsule run root changed within one provider", runtime.ErrCapsuleStateConflict)
	}
	if p.capsuleCatalogRoot != "" && p.capsuleCatalogRoot != capsule.CatalogMountPath {
		return fmt.Errorf("%w: SSH capsule catalog root changed within one provider", runtime.ErrCapsuleStateConflict)
	}
	p.capsuleRunRoot = capsule.RunRoot
	p.capsuleCatalogRoot = capsule.CatalogMountPath
	return nil
}

func (p *Provider) capsuleStateForStop(ctx context.Context, name, liveUID string, tmuxAlive bool) (runtime.CapsuleStateReference, bool, error) {
	if tmuxAlive && liveUID == "" {
		return runtime.CapsuleStateReference{}, false, nil
	}
	if !tmuxAlive {
		_, code, err := p.conn.Exec(ctx, name, []string{"test", "-d", p.capsuleStateRoot})
		if err != nil {
			return runtime.CapsuleStateReference{}, false, errors.Join(runtime.ErrRuntimeUnavailable, err)
		}
		if code == 1 {
			return runtime.CapsuleStateReference{}, false, nil
		}
		if code != 0 {
			return runtime.CapsuleStateReference{}, false, fmt.Errorf("%w: SSH capsule state root check exited %d", runtime.ErrCapsuleStateConflict, code)
		}
	}
	refs, err := p.ListCapsuleStates(ctx)
	if err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	var matches []runtime.CapsuleStateReference
	for _, ref := range refs {
		if ref.Key.SessionID == name && (liveUID == "" || ref.ResourceUID == liveUID) {
			matches = append(matches, ref)
		}
	}
	if len(matches) == 0 {
		if liveUID != "" {
			return runtime.CapsuleStateReference{}, false, fmt.Errorf("%w: live SSH capsule state has no exact allocation", runtime.ErrCapsuleStateConflict)
		}
		return runtime.CapsuleStateReference{}, false, nil
	}
	if len(matches) != 1 {
		return runtime.CapsuleStateReference{}, false, fmt.Errorf("%w: SSH capsule session identity is ambiguous", runtime.ErrCapsuleStateConflict)
	}
	return matches[0], true, nil
}

func (p *Provider) stopCapsule(ctx context.Context, name string, ref runtime.CapsuleStateReference) error {
	p.mu.RLock()
	runRoot := strings.TrimSpace(p.capsuleRunRoot)
	catalogRoot := strings.TrimSpace(p.capsuleCatalogRoot)
	p.mu.RUnlock()
	if runRoot == "" {
		runRoot = defaultCapsuleRunRoot
	}
	if catalogRoot == "" {
		catalogRoot = defaultCapsuleCatalogRoot
	}
	args := []string{
		"sh", "-c", remoteStopCapsuleScript, "gc-ssh-capsule-stop-v1",
		name, ref.ResourceUID, ref.MountPath, ref.ResourceID,
		runRoot, catalogRoot,
	}
	_, code, runErr := p.conn.Exec(ctx, name, args)
	if runErr != nil {
		// The exact kill and removals may have committed before the reply was
		// lost. The protocol is idempotent, so reconnect once and replay it.
		_, code, runErr = p.conn.Exec(ctx, name, args)
	}
	if runErr != nil {
		return fmt.Errorf("stop SSH capsule %q: %w", name, errors.Join(runtime.ErrRuntimeUnavailable, runErr))
	}
	if code != 0 {
		return fmt.Errorf("stop SSH capsule %q: %w: remote cleanup exited %d", name, runtime.ErrCapsuleStateConflict, code)
	}
	return nil
}

const remoteStopCapsuleScript = `
set -f
name=$1
expected_uid=$2
state_root=$3
resource=$4
run_root=$5
catalog_root=$6
case $name in ''|*[!A-Za-z0-9_-]*) exit 73 ;; esac
case $resource in ''|*[!a-z0-9-]*) exit 73 ;; esac
case $expected_uid in ''|*[!0-9:]*|:*|*:) exit 73 ;; esac
if stat -c %u "$state_root" >/dev/null 2>&1; then
  file_owner() { stat -c %u "$1"; }
  file_identity() { stat -c '%d:%i' "$1"; }
elif stat -f %u "$state_root" >/dev/null 2>&1; then
  file_owner() { stat -f %u "$1"; }
  file_identity() { stat -f '%d:%i' "$1"; }
else
  exit 74
fi
uid=$(id -u) || exit 74
state_path=$state_root/$resource
[ -d "$state_root" ] && [ ! -L "$state_root" ] && [ "$(file_owner "$state_root")" = "$uid" ] || exit 73
[ -d "$state_path" ] && [ ! -L "$state_path" ] && [ "$(file_owner "$state_path")" = "$uid" ] || exit 73
[ "$(file_identity "$state_path")" = "$expected_uid" ] || exit 73

pane_pid=
pane_pgid=
if tmux has-session -t "$name" >/dev/null 2>&1; then
  attached_uid=$(tmux show-options -qv -t "$name" ` + capsuleStateTMUXOption + `) || exit 74
  [ -z "$attached_uid" ] || [ "$attached_uid" = "$expected_uid" ] || exit 73
  pane_pid=$(tmux display-message -p -t "$name" '#{pane_pid}') || exit 74
  case $pane_pid in ''|*[!0-9]*) exit 74 ;; esac
  pane_pgid=$(ps -o pgid= -p "$pane_pid" 2>/dev/null | tr -d ' ') || exit 74
  pane_command=$(ps -o command= -p "$pane_pid" 2>/dev/null) || exit 74
  case $pane_pgid in ''|*[!0-9]*) exit 74 ;; esac
  case " $pane_command " in *" --state-root $state_path "*) ;; *) exit 73 ;; esac
  tmux kill-session -t "$name" || exit 74
else
  while read -r candidate_pid candidate_pgid candidate_command; do
    case $candidate_pid:$candidate_pgid in *[!0-9:]*) continue ;; esac
    case " $candidate_command " in
      *" --state-root $state_path "*)
        if [ -n "$pane_pgid" ] && [ "$pane_pgid" != "$candidate_pgid" ]; then exit 73; fi
        pane_pid=$candidate_pid
        pane_pgid=$candidate_pgid
        ;;
    esac
  done <<EOF
$(ps -eo pid=,pgid=,command= 2>/dev/null)
EOF
fi

if [ -n "$pane_pgid" ]; then
  self_pgid=$(ps -o pgid= -p $$ 2>/dev/null | tr -d ' ') || exit 74
  [ "$pane_pgid" -gt 1 ] && [ "$pane_pgid" != "$self_pgid" ] || exit 73
  if kill -0 -- "-$pane_pgid" 2>/dev/null; then
    kill -TERM -- "-$pane_pgid" 2>/dev/null || exit 74
    attempts=0
    while kill -0 -- "-$pane_pgid" 2>/dev/null && [ "$attempts" -lt 20 ]; do
      sleep 0.05
      attempts=$((attempts + 1))
    done
    if kill -0 -- "-$pane_pgid" 2>/dev/null; then
      kill -KILL -- "-$pane_pgid" 2>/dev/null || exit 74
    fi
  fi
fi

cleanup_tree() {
  root=$1
  target=$2
  [ -d "$root" ] && [ ! -L "$root" ] && [ "$(file_owner "$root")" = "$uid" ] || return 1
  if [ -e "$target" ] || [ -L "$target" ]; then
    [ -d "$target" ] && [ ! -L "$target" ] && [ "$(file_owner "$target")" = "$uid" ] || return 1
    rm -rf -- "$target" || return 1
  fi
}
run_path=$run_root/$resource
catalog_path=$catalog_root/$resource
cleanup_tree "$run_root" "$run_path" || exit 73
cleanup_tree "$catalog_root" "$catalog_path" || exit 73
`
