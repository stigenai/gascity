package herdr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	viewerBindingVersion = 1
	viewerWorkspaceLabel = "Gas City viewers"
)

// ViewerSpec identifies one lifecycle-neutral Herdr view of a Gas City
// session. Label and ProfileBlurb are public operator-facing metadata; neither
// field is used to select credentials or control the worker lifecycle.
type ViewerSpec struct {
	Session      string
	Label        string
	ProfileBlurb string
}

// ViewerBinding is the durable projection from a Gas City session to the
// Herdr pane that observes it. It deliberately contains no worker ownership,
// health, credentials, or conversation content.
type ViewerBinding struct {
	Version      int    `json:"version"`
	Session      string `json:"session"`
	PaneID       string `json:"pane_id"`
	TabID        string `json:"tab_id"`
	Label        string `json:"label,omitempty"`
	ProfileBlurb string `json:"profile_blurb,omitempty"`
}

// ViewerProjection owns only local Herdr viewer panes. The Gas City
// controller and the selected runtime provider remain the sole lifecycle
// owners of the sessions displayed inside those panes.
type ViewerProjection struct {
	c        *client
	stateDir string
	cityRoot string
	mu       sync.Mutex
}

// NewViewerProjection constructs a lifecycle-neutral viewer projection for a
// named Herdr server. stateDir stores viewer bindings separately from worker
// runtime metadata.
func NewViewerProjection(herdrSession, stateDir, cityRoot string) *ViewerProjection {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = filepath.Join(os.TempDir(), "gc-herdr-viewers", sanitize(herdrSession))
	}
	return newViewerProjection(newClient(herdrSession, cityRoot), stateDir, cityRoot)
}

func newViewerProjection(c *client, stateDir, cityRoot string) *ViewerProjection {
	return &ViewerProjection{c: c, stateDir: stateDir, cityRoot: cityRoot}
}

// Open creates, reuses, or reconnects the single viewer pane for spec.Session.
// It launches gc session attach in --no-resume mode, so missing, suspended,
// replaced, and unauthorized sessions surface in the pane without a start,
// stop, replacement, or health mutation from the viewer layer.
func (v *ViewerProjection) Open(ctx context.Context, spec ViewerSpec) (ViewerBinding, error) {
	spec.Session = strings.TrimSpace(spec.Session)
	if spec.Session == "" {
		return ViewerBinding{}, errors.New("herdr viewer: session is required")
	}
	if strings.ContainsRune(spec.Session, 0) {
		return ViewerBinding{}, errors.New("herdr viewer: session contains NUL")
	}
	if err := v.c.startServer(); err != nil {
		return ViewerBinding{}, fmt.Errorf("herdr viewer: configure server: %w", err)
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	rawCommand, argv := viewerAttachCommand(spec.Session)
	binding, present, err := v.readBinding(spec.Session)
	if err != nil {
		return ViewerBinding{}, err
	}
	if present {
		shellPID, foreground, probeErr := v.c.processInfo(ctx, binding.PaneID)
		probe := paneProbeFrom(shellPID, foreground)
		switch {
		case herdrErrorCode(probeErr) == "pane_not_found":
			if err := v.removeBinding(spec.Session); err != nil {
				return ViewerBinding{}, err
			}
			present = false
		case probeErr != nil:
			return ViewerBinding{}, fmt.Errorf("herdr viewer %q: probe pane %q: %w", spec.Session, binding.PaneID, probeErr)
		case viewerCommandMatches(foreground, rawCommand, argv):
			return v.refreshBinding(ctx, binding, spec)
		case probe.Busy || paneHasShellCommand(foreground):
			return ViewerBinding{}, fmt.Errorf("herdr viewer %q: bound pane %q runs a different command", spec.Session, binding.PaneID)
		default:
			if err := v.launch(ctx, binding.PaneID, rawCommand); err != nil {
				return ViewerBinding{}, fmt.Errorf("herdr viewer %q: reconnect pane %q: %w", spec.Session, binding.PaneID, err)
			}
			return v.refreshBinding(ctx, binding, spec)
		}
	}

	if !present {
		tabID, paneID, err := v.c.ensurePlacement(ctx, viewerWorkspaceLabel, viewerTabLabel(spec), v.cityRoot, nil)
		if err != nil {
			return ViewerBinding{}, fmt.Errorf("herdr viewer %q: place pane: %w", spec.Session, err)
		}
		if err := v.launch(ctx, paneID, rawCommand); err != nil {
			cause := fmt.Errorf("herdr viewer %q: launch attachment: %w", spec.Session, err)
			return ViewerBinding{}, v.rollbackTab(ctx, spec.Session, tabID, cause)
		}
		binding = ViewerBinding{
			Version: viewerBindingVersion, Session: spec.Session, PaneID: paneID, TabID: tabID,
			Label: spec.Label, ProfileBlurb: spec.ProfileBlurb,
		}
		if err := v.writeBinding(binding); err != nil {
			return ViewerBinding{}, v.rollbackTab(ctx, spec.Session, tabID, err)
		}
	}
	return binding, nil
}

// Close removes only the Herdr tab owned by the viewer projection. It refuses
// to close a live pane whose foreground no longer matches the recorded viewer
// attachment and never invokes the target runtime's Stop operation.
func (v *ViewerProjection) Close(ctx context.Context, session string) error {
	session = strings.TrimSpace(session)
	if session == "" {
		return errors.New("herdr viewer: session is required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	binding, present, err := v.readBinding(session)
	if err != nil || !present {
		return err
	}
	rawCommand, argv := viewerAttachCommand(session)
	shellPID, foreground, err := v.c.processInfo(ctx, binding.PaneID)
	probe := paneProbeFrom(shellPID, foreground)
	switch {
	case herdrErrorCode(err) == "pane_not_found":
		return v.removeBinding(session)
	case err != nil:
		return fmt.Errorf("herdr viewer %q: probe pane %q before close: %w", session, binding.PaneID, err)
	case viewerCommandMatches(foreground, rawCommand, argv):
		// The pane still runs this exact viewer and is safe to close.
	case probe.Busy || paneHasShellCommand(foreground):
		return fmt.Errorf("herdr viewer %q: bound pane %q runs a different command", session, binding.PaneID)
	}
	if err := v.c.tabClose(ctx, binding.TabID); err != nil && herdrErrorCode(err) != "tab_not_found" {
		return fmt.Errorf("herdr viewer %q: close tab %q: %w", session, binding.TabID, err)
	}
	return v.removeBinding(session)
}

// Binding reads the current durable viewer binding without treating it as a
// liveness signal. Call Open to live-probe and restore a view.
func (v *ViewerProjection) Binding(session string) (ViewerBinding, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.readBinding(strings.TrimSpace(session))
}

func (v *ViewerProjection) launch(ctx context.Context, paneID, rawCommand string) error {
	return v.c.paneRun(ctx, paneID, "exec /bin/sh -c "+shellquote.Quote(rawCommand))
}

func (v *ViewerProjection) rollbackTab(ctx context.Context, session, tabID string, cause error) error {
	err := v.c.tabClose(ctx, tabID)
	if err == nil || herdrErrorCode(err) == "tab_not_found" {
		return cause
	}
	return errors.Join(cause, fmt.Errorf("herdr viewer %q: roll back tab %q: %w", session, tabID, err))
}

func (v *ViewerProjection) refreshBinding(ctx context.Context, binding ViewerBinding, spec ViewerSpec) (ViewerBinding, error) {
	if binding.Label != spec.Label {
		if err := v.c.tabRename(ctx, binding.TabID, viewerTabLabel(spec)); err != nil {
			return ViewerBinding{}, fmt.Errorf("herdr viewer %q: rename tab %q: %w", spec.Session, binding.TabID, err)
		}
	}
	binding.Label = spec.Label
	binding.ProfileBlurb = spec.ProfileBlurb
	if err := v.writeBinding(binding); err != nil {
		return ViewerBinding{}, err
	}
	return binding, nil
}

func viewerAttachCommand(session string) (string, []string) {
	argv := []string{"gc", "session", "attach", "--no-resume", "--", session}
	return "exec " + shellquote.Join(argv), argv
}

func viewerCommandMatches(foreground []proc, raw string, argv []string) bool {
	if paneRunsCommand(foreground, raw) {
		return true
	}
	for _, process := range foreground {
		if len(process.Argv) != len(argv) || filepath.Base(process.Argv[0]) != argv[0] {
			continue
		}
		matched := true
		for i := 1; i < len(argv); i++ {
			if process.Argv[i] != argv[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func paneHasShellCommand(foreground []proc) bool {
	for _, process := range foreground {
		if len(process.Argv) >= 3 && strings.HasSuffix(process.Argv[0], "sh") && process.Argv[1] == "-c" {
			return true
		}
	}
	return false
}

func viewerTabLabel(spec ViewerSpec) string {
	if label := strings.TrimSpace(spec.Label); label != "" {
		return "view · " + label + " · " + spec.Session
	}
	return "view · " + spec.Session
}

func (v *ViewerProjection) bindingPath(session string) string {
	sum := sha256.Sum256([]byte(session))
	return filepath.Join(v.stateDir, hex.EncodeToString(sum[:])+".json")
}

func (v *ViewerProjection) readBinding(session string) (ViewerBinding, bool, error) {
	if session == "" {
		return ViewerBinding{}, false, nil
	}
	data, err := os.ReadFile(v.bindingPath(session))
	if errors.Is(err, os.ErrNotExist) {
		return ViewerBinding{}, false, nil
	}
	if err != nil {
		return ViewerBinding{}, false, fmt.Errorf("herdr viewer %q: read binding: %w", session, err)
	}
	var binding ViewerBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return ViewerBinding{}, false, fmt.Errorf("herdr viewer %q: decode binding: %w", session, err)
	}
	if binding.Version != viewerBindingVersion || binding.Session != session || strings.TrimSpace(binding.PaneID) == "" || strings.TrimSpace(binding.TabID) == "" {
		return ViewerBinding{}, false, fmt.Errorf("herdr viewer %q: invalid binding", session)
	}
	return binding, true, nil
}

func (v *ViewerProjection) writeBinding(binding ViewerBinding) error {
	data, err := json.Marshal(binding)
	if err != nil {
		return fmt.Errorf("herdr viewer %q: encode binding: %w", binding.Session, err)
	}
	if err := os.MkdirAll(v.stateDir, 0o700); err != nil {
		return fmt.Errorf("herdr viewer %q: create binding directory: %w", binding.Session, err)
	}
	tmp, err := os.CreateTemp(v.stateDir, ".viewer-*.tmp")
	if err != nil {
		return fmt.Errorf("herdr viewer %q: create binding temp file: %w", binding.Session, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		cause := fmt.Errorf("herdr viewer %q: protect binding temp file: %w", binding.Session, err)
		return errors.Join(cause, tmp.Close())
	}
	if _, err := tmp.Write(data); err != nil {
		cause := fmt.Errorf("herdr viewer %q: write binding: %w", binding.Session, err)
		return errors.Join(cause, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("herdr viewer %q: close binding: %w", binding.Session, err)
	}
	if err := os.Rename(tmpPath, v.bindingPath(binding.Session)); err != nil {
		return fmt.Errorf("herdr viewer %q: commit binding: %w", binding.Session, err)
	}
	return nil
}

func (v *ViewerProjection) removeBinding(session string) error {
	err := os.Remove(v.bindingPath(session))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("herdr viewer %q: remove binding: %w", session, err)
	}
	return nil
}
