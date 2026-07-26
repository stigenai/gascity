package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ── pane binding: the stable agent handle under herdr ≥0.7.4 ─────────────────
//
// herdr ≥0.7.4 clears an agent's *name* from its registry when the pane
// occupant exits, is released, or is replaced. On 0.7.5 that is by design —
// `agent start` detects the launched TUI and a cleared name means the agent
// exited — but it also means every name-keyed lookup can go dark while the
// session's pane lives on (raw shell sessions are never registered at all).
// Reading a live session as absent is the spawn storm: IsRunning goes false,
// the reconciler re-Starts every tick, and each wrongful Start leaks a pane.
// The *pane id* is the stable handle, so Start persists it (plus the launch
// mode) in the metadata sidecar and every name→pane resolution falls back to
// it, probed live before it is trusted (pane ids recycle).

// Sidecar keys for the placement herdr assigned at Start. Namespaced away from
// the GC_* env keys seedMetaFromEnv mirrors into the same store.
const (
	metaBoundPane      = "GC_HERDR_PANE_ID"
	metaBoundTab       = "GC_HERDR_TAB_ID"
	metaBoundWorkspace = "GC_HERDR_WORKSPACE_ID"
	metaBoundMode      = "GC_HERDR_LAUNCH_MODE"
	// metaBoundName holds the exact session name (sidecar directories use the
	// sanitized form, which is lossy), so ListRunning can enumerate bound
	// sessions that herdr's registry does not know about.
	metaBoundName = "GC_HERDR_SESSION_NAME"
)

// Launch modes persisted at metaBoundMode. They pick the liveness rule for
// the binding fallback: a registered agent (bindModeAgent) whose pane is back
// at a bare shell prompt has exited — its pane still resolves so Stop can
// close it, but the session is not running; a raw/bare shell session
// (bindModeShell) runs as long as its pane exists, because `exec /bin/sh -c`
// panes die with their command.
const (
	bindModeAgent = "agent"
	bindModeShell = "shell"
)

// paneProbe is what probing a bound pane learned: whether the pane still
// exists, and whether something beyond the pane's own shell is in the
// foreground (a foreground process with a pid other than the shell's).
type paneProbe struct {
	Exists bool
	Busy   bool
}

// paneLookupOps are the operations resolveBinding needs, injected as closures
// so the resolution decision is unit-testable without a live herdr server
// (mirrors agentStartOps).
type paneLookupOps struct {
	// getAgent is the name-keyed registry lookup (fast path while the name lives).
	getAgent func() (agentInfo, bool, error)
	// boundPane reads the sidecar pane binding ("" when absent).
	boundPane func() string
	// boundMode reads the persisted launch mode ("" on pre-upgrade bindings).
	boundMode func() string
	// probePane inspects the bound pane. A zero probe with nil error means
	// herdr confirmed the pane gone; a non-nil error means the probe itself
	// failed (transport), which proves nothing either way.
	probePane func(paneID string) (paneProbe, error)
	// clearBinding drops a binding whose pane herdr confirmed gone, so a
	// recycled pane id can never resurrect a dead session.
	clearBinding func()
}

// resolveBinding resolves a session name to its herdr pane id and a running
// verdict: registry name lookup first (a live name is a running agent), then
// the sidecar pane binding, trusted only after a live probe. The pane resolves
// whenever it exists (Stop/keys/read need the handle even for an exited
// agent); running is mode-aware — busy always runs, a bare shell prompt runs
// only for bindModeShell. A binding whose pane is confirmed gone is cleared
// and resolves absent; a transport failure on either tier surfaces as an
// error and clears nothing.
func resolveBinding(ops paneLookupOps) (paneID string, running bool, err error) {
	a, ok, err := ops.getAgent()
	if err != nil {
		return "", false, err
	}
	if ok && a.PaneID != "" {
		return a.PaneID, true, nil
	}
	pane := strings.TrimSpace(ops.boundPane())
	if pane == "" {
		return "", false, nil
	}
	probe, err := ops.probePane(pane)
	if err != nil {
		return "", false, err
	}
	if !probe.Exists {
		ops.clearBinding()
		return "", false, nil
	}
	return pane, probe.Busy || ops.boundMode() == bindModeShell, nil
}

// bindPlacement persists the placement herdr assigned this agent plus its
// launch mode, so every later name-keyed op survives the name clear. Called
// by Start after the agent (fresh or adopted) is up; Stop's clearMeta
// removes it.
func (p *Provider) bindPlacement(name string, info agentInfo, mode string) error {
	for key, val := range map[string]string{
		metaBoundPane:      info.PaneID,
		metaBoundTab:       info.TabID,
		metaBoundWorkspace: info.WorkspaceID,
		metaBoundMode:      mode,
		metaBoundName:      name,
	} {
		if val == "" {
			continue
		}
		if err := p.SetMeta(name, key, val); err != nil {
			return err
		}
	}
	return nil
}

// clearPaneBinding drops the persisted placement (not the whole sidecar — the
// session identity keys stay for the reconciler). Idempotent.
func (p *Provider) clearPaneBinding(name string) {
	_ = p.RemoveMeta(name, metaBoundPane)
	_ = p.RemoveMeta(name, metaBoundTab)
	_ = p.RemoveMeta(name, metaBoundWorkspace)
	_ = p.RemoveMeta(name, metaBoundMode)
	_ = p.RemoveMeta(name, metaBoundName)
}

// boundSessionNames enumerates the session names with a live-looking sidecar
// binding (a stored name and pane id), for ListRunning to merge with herdr's
// registry — which never sees raw shell sessions.
func (p *Provider) boundSessionNames() []string {
	entries, err := os.ReadDir(p.metaDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, err := readMetaFile(filepath.Join(p.metaDir, e.Name(), sanitize(metaBoundName)))
		if err != nil || name == "" {
			continue
		}
		if pane, err := readMetaFile(filepath.Join(p.metaDir, e.Name(), sanitize(metaBoundPane))); err != nil || pane == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// readMetaFile reads one sidecar value ("" when absent).
func readMetaFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// probePane inspects a bound pane via `pane process-info`. herdr answering
// not-found is a confirmed-gone (zero probe, nil error); any other failure is
// a transport error that proves nothing.
func (p *Provider) probePane(ctx context.Context, paneID string) (paneProbe, error) {
	shellPID, fg, err := p.c.processInfo(ctx, paneID)
	if err != nil {
		if strings.Contains(err.Error(), "not_found") || strings.Contains(err.Error(), "not found") {
			return paneProbe{}, nil
		}
		return paneProbe{}, err
	}
	return paneProbeFrom(shellPID, fg), nil
}

// interactiveShells are the interactive shells a fresh pane idles in; a pane
// whose root foreground process is one of these (and nothing else runs) is at
// a bare prompt.
var interactiveShells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"ksh": true, "tcsh": true, "csh": true,
}

// paneProbeFrom folds process-info into the probe verdict. Busy means the
// pane is running something beyond an interactive shell prompt: a foreground
// process other than the root (a launched agent or a shell job), or a root
// that is no longer a shell at all (`exec`'d commands replace it, keeping its
// pid). This is the version-robust "is the session still in there" signal —
// matching configured process names is not (claude ≥2.1.x reports comm as
// its bare version string).
func paneProbeFrom(shellPID int, fg []proc) paneProbe {
	probe := paneProbe{Exists: shellPID != 0}
	for _, pr := range fg {
		if pr.PID == 0 {
			continue
		}
		if pr.PID != shellPID || !interactiveShells[strings.TrimPrefix(pr.Name, "-")] {
			probe.Busy = true
			break
		}
	}
	return probe
}

// paneRunsCommand reports whether a pane's foreground holds the launched
// `/bin/sh -c <raw>` wrapper (exec preserves argv) — the positive signal that
// a typed raw launch actually executed, immune to the shell-init children a
// fresh pane runs first.
func paneRunsCommand(fg []proc, raw string) bool {
	for _, pr := range fg {
		if len(pr.Argv) >= 3 && strings.HasSuffix(pr.Argv[0], "sh") && pr.Argv[1] == "-c" && pr.Argv[2] == raw {
			return true
		}
	}
	return false
}

// paneRootReplaced reports whether the pane's root process (pid == shellPID)
// is visible in the foreground and is no longer an interactive shell — a raw
// launch that exec'd straight through the `/bin/sh -c` wrapper (e.g.
// `exec sleep 120`). Shell-init children keep the root a shell, so they never
// read as replaced.
func paneRootReplaced(shellPID int, fg []proc) bool {
	for _, pr := range fg {
		if pr.PID == shellPID {
			return !interactiveShells[strings.TrimPrefix(pr.Name, "-")]
		}
	}
	return false
}

// lookupOps wires paneLookupOps for a session name.
func (p *Provider) lookupOps(ctx context.Context, name string) paneLookupOps {
	meta := func(key string) string {
		v, err := p.GetMeta(name, key)
		if err != nil {
			return ""
		}
		return v
	}
	return paneLookupOps{
		getAgent:     func() (agentInfo, bool, error) { return p.c.getAgent(ctx, name) },
		boundPane:    func() string { return meta(metaBoundPane) },
		boundMode:    func() string { return strings.TrimSpace(meta(metaBoundMode)) },
		probePane:    func(paneID string) (paneProbe, error) { return p.probePane(ctx, paneID) },
		clearBinding: func() { p.clearPaneBinding(name) },
	}
}
