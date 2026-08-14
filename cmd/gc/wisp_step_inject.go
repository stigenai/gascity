package main

import (
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// wispStepInjectedToMetadataKey records the session ID that last received a
// bead's title+description via wisp-step injection. wispStepInjectedFingerprintMetadataKey
// records a fingerprint of that content at the time. Together they let a
// long-running session's per-turn nudge hook skip re-rendering an unchanged
// assigned bead into context on every wake (gcy-5rkt) — keyed by session ID
// rather than bead assignee so a recycled/restarted successor, which carries
// a new session ID even under the same role or alias, still gets its own
// first read. wispStepInjectedAtMetadataKey records when that marker was
// stamped; wispStepReinjectionMaxAge bounds how long the dedup can suppress
// re-injection regardless of the marker still matching, since GC_SESSION_ID
// persists across an in-harness context compaction/summarization (no new
// SessionStart), so a long-lived single-step session could otherwise go
// indefinitely without ever seeing its assignment reminder again if a
// compaction doesn't preserve it with full fidelity (gcy-fn0b). Chosen to
// stay well above ordinary nudge cadence (~12min, the exact waste gcy-5rkt
// fixed) so this backstop doesn't reintroduce it.
const (
	wispStepInjectedToMetadataKey          = "wisp_step_injected_to_session"
	wispStepInjectedFingerprintMetadataKey = "wisp_step_injected_fingerprint"
	wispStepInjectedAtMetadataKey          = "wisp_step_injected_at"
	wispStepReinjectionMaxAge              = 2 * time.Hour
)

// wispStepInjectionContent resolves the agent's current in-progress formula
// step bead and returns it formatted as a <system-reminder> block, or "" if
// none is found or any error occurs. Designed for best-effort use in hook
// injection paths — callers must never fail hard on an empty return.
//
// Store priority: if GC_RIG_ROOT is set the rig store is queried (where
// rig-scoped polecat work beads live), otherwise the city store at cityPath.
// When cityPath is empty the function falls back to GC_CITY from the env.
func wispStepInjectionContent(cityPath string) string {
	effective := cityPath
	if effective == "" {
		effective = strings.TrimSpace(os.Getenv("GC_CITY"))
	}
	store := openWispStepStore(effective)
	if store == nil {
		return ""
	}
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	return wispStepInjectionContentForStore(store, wispStepAssignees(), sessionID)
}

// wispStepInjectionContentForStore is wispStepInjectionContent's testable
// core: resolve the active step, skip it if this exact content was already
// injected to this exact session, otherwise stamp the marker and render it.
// sessionID == "" (no session context available) always injects, the same
// as the pre-dedup behavior — a missing cache key must never suppress a read.
func wispStepInjectionContentForStore(store beads.Store, assignees []string, sessionID string) string {
	if len(assignees) == 0 {
		return ""
	}
	b, err := resolveActiveWispStep(store, assignees)
	if err != nil || b == nil {
		return ""
	}
	if sessionID != "" {
		if wispStepAlreadyInjected(b, sessionID) {
			return ""
		}
		stampWispStepInjected(store, b, sessionID)
	}
	return formatWispStepReminder(b)
}

// wispStepAlreadyInjected reports whether b's current title+description have
// already been injected to sessionID, per the markers stamped by
// stampWispStepInjected, and that marker is still within
// wispStepReinjectionMaxAge. A missing or unparseable timestamp (a marker
// stamped before this field existed) is treated as fresh rather than forcing
// a one-time mass re-injection fleet-wide the first time this ships.
func wispStepAlreadyInjected(b *beads.Bead, sessionID string) bool {
	if b.Metadata[wispStepInjectedToMetadataKey] != sessionID ||
		b.Metadata[wispStepInjectedFingerprintMetadataKey] != wispStepContentFingerprint(b) {
		return false
	}
	injectedAt, err := time.Parse(time.RFC3339, b.Metadata[wispStepInjectedAtMetadataKey])
	if err != nil {
		return true
	}
	return time.Since(injectedAt) < wispStepReinjectionMaxAge
}

// stampWispStepInjected records that b's current content has been shown to
// sessionID, and when. Best-effort: a write failure just costs one extra
// repeat injection next turn, not a broken hook, so it only logs.
func stampWispStepInjected(store beads.Store, b *beads.Bead, sessionID string) {
	err := store.SetMetadataBatch(b.ID, map[string]string{
		wispStepInjectedToMetadataKey:          sessionID,
		wispStepInjectedFingerprintMetadataKey: wispStepContentFingerprint(b),
		wispStepInjectedAtMetadataKey:          time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		log.Printf("wisp step inject: stamping injected marker on %s: %v", b.ID, err)
	}
}

// wispStepContentFingerprint hashes b's title+description so a repeat-
// injection check doesn't have to store a second full-size copy of
// (potentially multi-KB) description text as bead metadata just to compare it.
func wispStepContentFingerprint(b *beads.Bead) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(b.Title))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(b.Description))
	return strconv.FormatUint(h.Sum64(), 16)
}

// openWispStepStore opens the bead store to query for active wisp steps.
// If GC_RIG_ROOT is set it opens that rig's store (where rig-scoped polecat
// work lives); otherwise it opens the city store at cityPath.
// Returns nil on any error — callers treat nil as "no store available".
func openWispStepStore(cityPath string) beads.Store {
	if rigRoot := strings.TrimSpace(os.Getenv("GC_RIG_ROOT")); rigRoot != "" {
		store, err := openStoreAtForCity(rigRoot, cityPath)
		if err == nil {
			return store
		}
	}
	if cityPath == "" {
		return nil
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil
	}
	return store
}

// wispStepAssignees returns the deduped set of identity strings to match
// against bead assignees. Uses GC_ALIAS (primary), GC_SESSION_NAME, and
// GC_SESSION_ID in that priority order.
func wispStepAssignees() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(os.Getenv("GC_ALIAS"))
	add(os.Getenv("GC_SESSION_NAME"))
	add(os.Getenv("GC_SESSION_ID"))
	return out
}

// resolveActiveWispStep returns the agent's current formula step bead.
//
// Resolution order:
//  1. Find the agent's in-progress molecule bead (type=molecule or type=wisp).
//  2. Find that molecule's in-progress type=step child — the current step.
//  3. If no in-progress step child exists, fall back to the entry step: the
//     first open type=step child (deterministic formula start position).
//  4. If no molecule bead is assigned to the agent, follow the molecule_id
//     bridge: an attached (v1) formula routes only the source work bead and
//     stamps its molecule_id with the (unrouted, unassigned) root, so resolve
//     the root's active step through that bridge.
//  5. If no molecule_id bridge exists either, fall back to any in-progress bead
//     with a non-empty Description (legacy behavior for agents not running a
//     formula).
//
// Returns nil, nil when no bead can be resolved. Never returns an error for
// not-found conditions — callers treat nil as "nothing to inject".
func resolveActiveWispStep(store beads.Store, assignees []string) (*beads.Bead, error) {
	if store == nil || len(assignees) == 0 {
		return nil, nil
	}

	molecule, err := resolveActiveMolecule(store, assignees)
	if err != nil {
		return nil, err
	}
	if molecule == nil {
		// No molecule root is assigned to the agent. Attached (v1) formulas
		// leave the root unrouted and stamp molecule_id on the routed source
		// bead, so follow that bridge to the root's active step before the
		// legacy description fallback. Best-effort: a resolution error or no
		// bridge drops to legacy.
		if root := resolveMoleculeRootViaBridge(store, assignees); root != nil {
			step, stepErr := resolveInProgressStepChild(store, root.ID)
			if stepErr != nil {
				log.Printf("wisp step inject: error resolving in-progress step for bridged molecule %s: %v", root.ID, stepErr)
				return nil, nil
			}
			if step != nil {
				return step, nil
			}
			return resolveEntryStepChild(store, root.ID)
		}
		// No molecule bridge; fall back to legacy: any in-progress bead with a description.
		return resolveBeadWithDescription(store, assignees)
	}

	// Prefer the in-progress step child (the agent is mid-step).
	step, err := resolveInProgressStepChild(store, molecule.ID)
	if err != nil {
		log.Printf("wisp step inject: error resolving in-progress step children for molecule %s: %v", molecule.ID, err)
		return nil, nil
	}
	if step != nil {
		return step, nil
	}

	// Fall back to the entry step: first open step child.
	log.Printf("wisp step inject: no in-progress step for molecule %s; resolving entry step", molecule.ID)
	return resolveEntryStepChild(store, molecule.ID)
}

// resolveActiveMolecule returns the agent's in-progress molecule bead.
// When multiple molecules are found, the most recently updated one is returned
// and the ambiguity is logged. Returns nil, nil when none is found.
func resolveActiveMolecule(store beads.Store, assignees []string) (*beads.Bead, error) {
	for _, molType := range []string{"molecule", "wisp"} {
		results, err := store.List(beads.ListQuery{
			Status:    "in_progress",
			Type:      molType,
			Assignees: assignees,
			TierMode:  beads.TierBoth,
			Limit:     5,
		})
		if err != nil {
			return nil, fmt.Errorf("listing in-progress %s beads: %w", molType, err)
		}
		if len(results) == 0 {
			continue
		}
		if len(results) > 1 {
			ids := make([]string, len(results))
			for i, r := range results {
				ids[i] = r.ID
			}
			log.Printf("wisp step inject: %d in-progress %s beads found (%s); using most recent", len(results), molType, strings.Join(ids, ", "))
		}
		best := results[0]
		for _, r := range results[1:] {
			if r.UpdatedAt.After(best.UpdatedAt) {
				best = r
			}
		}
		return &best, nil
	}
	return nil, nil
}

// resolveMoleculeRootViaBridge finds the molecule root reachable from an
// attached (v1) source work bead. Attached formulas route only the source bead
// and stamp its molecule_id metadata with the (unrouted, unassigned) molecule
// root, so resolveActiveMolecule — which filters molecule roots by assignee —
// never matches. This bridges from the routed, assignee-owned source bead to
// its root via the molecule_id metadata key.
//
// Returns nil on any error or when no bridge bead is found — callers treat nil
// as "no bridge available" and fall through to the legacy path.
func resolveMoleculeRootViaBridge(store beads.Store, assignees []string) *beads.Bead {
	results, err := store.List(beads.ListQuery{
		Status:    "in_progress",
		Assignees: assignees,
		TierMode:  beads.TierBoth,
		Limit:     10,
	})
	if err != nil {
		return nil
	}
	for i := range results {
		rootID := strings.TrimSpace(results[i].Metadata[beadmeta.MoleculeIDMetadataKey])
		if rootID == "" {
			continue
		}
		root, err := store.Get(rootID)
		if err != nil {
			log.Printf("wisp step inject: molecule_id %q on bead %s did not resolve: %v", rootID, results[i].ID, err)
			continue
		}
		return &root
	}
	return nil
}

// resolveInProgressStepChild returns the in-progress type=step child of moleculeID.
// When multiple are found, the most recently updated one is returned.
func resolveInProgressStepChild(store beads.Store, moleculeID string) (*beads.Bead, error) {
	results, err := store.List(beads.ListQuery{
		Status:   "in_progress",
		Type:     "step",
		ParentID: moleculeID,
		TierMode: beads.TierBoth,
		Limit:    5,
	})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	if len(results) > 1 {
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ID
		}
		log.Printf("wisp step inject: %d in-progress steps for molecule %s (%s); using most recent", len(results), moleculeID, strings.Join(ids, ", "))
	}
	best := results[0]
	for _, r := range results[1:] {
		if r.UpdatedAt.After(best.UpdatedAt) {
			best = r
		}
	}
	return &best, nil
}

// resolveEntryStepChild returns the first open type=step child of moleculeID.
// This is the deterministic fallback when no step is in-progress: the formula's
// entry position — where execution should (re)start.
func resolveEntryStepChild(store beads.Store, moleculeID string) (*beads.Bead, error) {
	results, err := store.List(beads.ListQuery{
		Status:   "open",
		Type:     "step",
		ParentID: moleculeID,
		TierMode: beads.TierBoth,
		Limit:    1,
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving entry step for molecule %s: %w", moleculeID, err)
	}
	if len(results) == 0 {
		return nil, nil
	}
	b := results[0]
	return &b, nil
}

// resolveBeadWithDescription returns the first in-progress bead assigned to any
// of the given identities that has a non-empty Description. This is the legacy
// resolution path used when no molecule bead is assigned to the agent.
func resolveBeadWithDescription(store beads.Store, assignees []string) (*beads.Bead, error) {
	results, err := store.List(beads.ListQuery{
		Status:    "in_progress",
		Assignees: assignees,
		TierMode:  beads.TierBoth,
		Limit:     10,
	})
	if err != nil {
		return nil, err
	}
	for i := range results {
		if strings.TrimSpace(results[i].Description) != "" {
			b := results[i]
			return &b, nil
		}
	}
	return nil, nil
}

// formatWispStepReminder formats a formula step bead as a <system-reminder>
// block for injection into agent context.
func formatWispStepReminder(b *beads.Bead) string {
	title := extmsg.SanitizeForSystemReminder(strings.TrimSpace(b.Title))
	desc := extmsg.SanitizeForSystemReminder(strings.TrimSpace(b.Description))
	return fmt.Sprintf(
		"<system-reminder>\nYour current active work assignment:\n\n## %s (%s)\n\n%s\n</system-reminder>\n",
		title, b.ID, desc,
	)
}
