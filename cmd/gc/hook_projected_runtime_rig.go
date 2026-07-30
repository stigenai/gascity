package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// bindProjectedRuntimeRig restores the deployment-specific path intentionally
// omitted from reusable city configuration inside a managed session runtime.
//
// Kubernetes workers receive a single rig checkout projected beneath the city
// mount. The controller publishes that binding through the GC_RIG* and
// GC_STORE* environment contract. This function validates the complete
// contract and applies it only to the in-memory config used by gc hook. It
// never persists the path and never replaces a different authored binding.
func bindProjectedRuntimeRig(cityPath string, cfg *config.City) error {
	rigName := strings.TrimSpace(os.Getenv("GC_RIG"))
	rigRoot := strings.TrimSpace(os.Getenv("GC_RIG_ROOT"))
	storeScope := strings.TrimSpace(os.Getenv("GC_STORE_SCOPE"))

	// City-scoped managed sessions do not carry a rig binding.
	if rigName == "" && rigRoot == "" && storeScope != "rig" {
		return nil
	}
	if strings.TrimSpace(os.Getenv("GC_SESSION_ID")) == "" &&
		strings.TrimSpace(os.Getenv("GC_SESSION_NAME")) == "" {
		return fmt.Errorf("projected rig binding requires a managed session identity")
	}
	if cfg == nil {
		return fmt.Errorf("projected rig binding requires loaded city configuration")
	}
	if rigName == "" {
		return fmt.Errorf("projected rig binding requires GC_RIG")
	}
	if rigRoot == "" {
		return fmt.Errorf("projected rig binding requires GC_RIG_ROOT")
	}
	if storeScope != "rig" {
		return fmt.Errorf("projected rig binding requires GC_STORE_SCOPE=rig, got %q", storeScope)
	}
	if !filepath.IsAbs(rigRoot) {
		return fmt.Errorf("projected GC_RIG_ROOT must be absolute: %q", rigRoot)
	}

	cleanCity, err := filepath.Abs(filepath.Clean(cityPath))
	if err != nil {
		return fmt.Errorf("resolve city root: %w", err)
	}
	cleanRig := filepath.Clean(rigRoot)
	resolvedCity, err := filepath.EvalSymlinks(cleanCity)
	if err != nil {
		return fmt.Errorf("resolve city root %q: %w", cleanCity, err)
	}
	resolvedRig, err := filepath.EvalSymlinks(cleanRig)
	if err != nil {
		return fmt.Errorf("resolve projected GC_RIG_ROOT %q: %w", cleanRig, err)
	}
	rel, err := filepath.Rel(resolvedCity, resolvedRig)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("projected GC_RIG_ROOT %q must be inside city root %q", cleanRig, cleanCity)
	}

	storeRoot := strings.TrimSpace(os.Getenv("GC_STORE_ROOT"))
	if storeRoot == "" || !filepath.IsAbs(storeRoot) {
		return fmt.Errorf("projected rig binding requires absolute GC_STORE_ROOT matching GC_RIG_ROOT")
	}
	resolvedStore, err := filepath.EvalSymlinks(filepath.Clean(storeRoot))
	if err != nil || resolvedStore != resolvedRig {
		return fmt.Errorf("projected GC_STORE_ROOT %q must match GC_RIG_ROOT %q", storeRoot, cleanRig)
	}

	var rig *config.Rig
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			rig = &cfg.Rigs[i]
			break
		}
	}
	if rig == nil {
		return fmt.Errorf("projected rig %q is not declared in city configuration", rigName)
	}
	if prefix := strings.TrimSpace(os.Getenv("GC_BEADS_PREFIX")); prefix != rig.EffectivePrefix() {
		return fmt.Errorf(
			"projected GC_BEADS_PREFIX %q does not match declared rig %q prefix %q",
			prefix,
			rigName,
			rig.EffectivePrefix(),
		)
	}
	if configured := strings.TrimSpace(rig.Path); configured != "" {
		resolvedConfigured, resolveErr := filepath.EvalSymlinks(filepath.Clean(configured))
		if resolveErr != nil || resolvedConfigured != resolvedRig {
			return fmt.Errorf(
				"projected rig %q already has configured path %q; refusing runtime override with %q",
				rigName,
				configured,
				cleanRig,
			)
		}
		return nil
	}

	metadataPath := filepath.Join(cleanRig, ".beads", "metadata.json")
	info, err := os.Stat(metadataPath)
	if err != nil {
		return fmt.Errorf("validate projected rig metadata.json at %q: %w", metadataPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("projected rig metadata.json at %q is not a regular file", metadataPath)
	}

	rig.Path = cleanRig
	return nil
}
