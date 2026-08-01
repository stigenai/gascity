package molecule

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

var protectedRootMetadataKeys = func() map[string]struct{} {
	keys := make(map[string]struct{}, 9)
	for _, key := range []string{
		"idempotency_key",
		"workflow_id",
		"rig_root",
		beadmeta.MoleculeIDMetadataKey,
		beadmeta.MoleculeFailedMetadataKey,
		beadmeta.MergeStrategyMetadataKey,
		beadmeta.WorkerDirMetadataKey,
		beadmeta.ArtifactDirMetadataKey,
		beadmeta.LegacyWorkDirMetadataKey,
	} {
		keys[key] = struct{}{}
	}
	return keys
}()

// ValidateRootMetadata rejects caller metadata that could replace engine-owned
// provenance, graph identity, topology, routing, execution, or lifecycle
// state. The entire beadmeta.Namespace ("gc.") is reserved for the engine,
// including unknown future keys. Callers must invoke this before opening or
// reading a bead store. Caller-owned annotation namespaces remain allowed.
func ValidateRootMetadata(metadata map[string]string) error {
	if len(metadata) == 0 {
		return nil
	}
	protected := make([]string, 0)
	for key := range metadata {
		if isProtectedRootMetadataKey(key) {
			protected = append(protected, key)
		}
	}
	if len(protected) == 0 {
		return nil
	}
	sort.Strings(protected)
	quoted := make([]string, len(protected))
	for i, key := range protected {
		quoted[i] = strconv.Quote(key)
	}
	return fmt.Errorf("root metadata contains protected engine-owned keys: %s", strings.Join(quoted, ", "))
}

// ValidateExistingRootMetadata requires every caller-supplied metadata key to
// be explicitly present on an existing root with the exact requested value.
// Presence is part of the contract: an absent key is not equivalent to a key
// whose value is the empty string. Idempotent reuse paths call this before
// adopting an existing graph so a retry cannot silently weaken its original
// admission metadata.
func ValidateExistingRootMetadata(root beads.Bead, expected map[string]string) error {
	if len(expected) == 0 {
		return nil
	}
	keys := make([]string, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		want := expected[key]
		got, present := root.Metadata[key]
		if !present {
			return fmt.Errorf("existing root %s metadata mismatch for %q: key is absent, want %q", root.ID, key, want)
		}
		if got != want {
			return fmt.Errorf("existing root %s metadata mismatch for %q: got %q, want %q", root.ID, key, got, want)
		}
	}
	return nil
}

func isProtectedRootMetadataKey(key string) bool {
	if strings.HasPrefix(key, beadmeta.Namespace) {
		return true
	}
	if _, protected := protectedRootMetadataKeys[key]; protected {
		return true
	}
	return strings.HasPrefix(key, beadmeta.OptionMetadataPrefix)
}
