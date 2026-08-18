package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/omnigent"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// materializeOmnigentCapsule projects a typed provider declaration into the
// selected remote runtime. Local tmux and Herdr sessions keep using the
// controller-local Omnigent service and perform no remote state mutation.
func materializeOmnigentCapsule(bp *agentBuildParams, tp TemplateParams) (TemplateParams, error) {
	if bp == nil || tp.ResolvedProvider == nil || tp.ResolvedProvider.Capsule.Kind == "" {
		return tp, nil
	}
	capsuleSpec := tp.ResolvedProvider.Capsule
	if capsuleSpec.Kind != "omnigent" {
		return TemplateParams{}, fmt.Errorf("provider %q: unsupported capsule kind %q", tp.ResolvedProvider.Name, capsuleSpec.Kind)
	}

	runtimeName := strings.TrimSpace(tp.EffectiveSessionProvider)
	hybridRouteSet := false
	hybridRemote := false
	var routedState runtime.CapsuleStateRuntime
	var routedCityScope string
	if runtimeName == "hybrid" {
		resolver, ok := bp.sp.(runtime.CapsuleRouteResolver)
		if !ok {
			return TemplateParams{}, fmt.Errorf("agent %q: hybrid runtime does not expose capsule route resolution", tp.DisplayName())
		}
		var routeErr error
		hybridRemote, routedState, routedCityScope, routeErr = resolver.ResolveCapsuleRoute(tp.SessionName)
		if routeErr != nil {
			return TemplateParams{}, fmt.Errorf("agent %q: resolve hybrid capsule route: %w", tp.DisplayName(), routeErr)
		}
		hybridRouteSet = true
	}
	location, _, err := omnigent.ResolveAttachmentPlacement(runtimeName, hybridRouteSet, hybridRemote)
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: resolve Omnigent placement: %w", tp.DisplayName(), err)
	}
	if location == omnigent.AttachmentLocationController {
		return tp, nil
	}

	catalogPath, err := cityContainedOmnigentCatalogPath(bp.cityPath, capsuleSpec.Catalog)
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: resolve Omnigent catalog: %w", tp.DisplayName(), err)
	}
	catalog, err := omnigent.LoadCatalog(catalogPath)
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: load Omnigent catalog: %w", tp.DisplayName(), err)
	}
	catalogSHA256, err := omnigent.CatalogBundleSHA256(catalogPath)
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: hash Omnigent catalog: %w", tp.DisplayName(), err)
	}

	profile, err := omnigent.SelectProfile(
		tp.ResolvedProvider.EffectiveDefaults[capsuleSpec.ProfileOption],
		func(string) (string, bool) { return "", false },
	)
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: select Omnigent profile: %w", tp.DisplayName(), err)
	}

	cityScope := strings.TrimSpace(routedCityScope)
	if cityScope == "" {
		cityScope = strings.TrimSpace(bp.cityName)
	}
	if runtimeName != "hybrid" {
		if scoped, ok := bp.sp.(runtime.CapsuleCityScopeProvider); ok {
			cityScope = strings.TrimSpace(scoped.CapsuleCityScope())
			if cityScope == "" {
				return TemplateParams{}, fmt.Errorf("agent %q: capsule provider returned an empty city scope", tp.DisplayName())
			}
		}
	}
	if cityScope == "" {
		return TemplateParams{}, fmt.Errorf("agent %q: capsule city scope is empty", tp.DisplayName())
	}
	sessionID := strings.TrimSpace(tp.Env["GC_SESSION_ID"])
	if sessionID == "" {
		sessionID = strings.TrimSpace(tp.SessionName)
	}

	plan, err := omnigent.ResolveAttachmentLaunchPlan(omnigent.AttachmentLaunchInput{
		Runtime:          runtimeName,
		HybridRouteSet:   hybridRouteSet,
		HybridRemote:     hybridRemote,
		ProfileID:        profile.ID,
		Catalog:          catalog,
		Workspace:        tp.WorkDir,
		CityScope:        cityScope,
		SessionID:        sessionID,
		StateRoot:        omnigent.CapsuleStateRoot,
		SocketPath:       omnigent.CapsuleSocketPath,
		CatalogPath:      omnigent.CapsuleCatalogPath,
		CatalogSHA256:    catalogSHA256,
		Pin:              catalog.Pin,
		SecretReferences: tp.SecretReferences,
	})
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: resolve Omnigent capsule launch: %w", tp.DisplayName(), err)
	}

	stateRuntime := routedState
	if stateRuntime == nil {
		var ok bool
		stateRuntime, ok = bp.sp.(runtime.CapsuleStateRuntime)
		if !ok {
			return TemplateParams{}, fmt.Errorf("agent %q: selected runtime %q does not provide capsule state", tp.DisplayName(), runtimeName)
		}
	}
	state, _, err := stateRuntime.EnsureCapsuleState(context.Background(), plan.CapsuleKey)
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: ensure Omnigent capsule state: %w", tp.DisplayName(), err)
	}

	capsule, err := plan.RuntimeCapsuleConfig(state, omnigentCatalogResourceID(plan))
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: build Omnigent runtime capsule: %w", tp.DisplayName(), err)
	}
	if err := capsule.Validate(); err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: validate Omnigent runtime capsule: %w", tp.DisplayName(), err)
	}

	tp.Capsule = capsule
	tp.Command = shellquote.Join(capsule.Command)
	tp.SecretReferences = append([]runtime.SecretReference(nil), plan.SecretReferences...)
	return tp, nil
}

func cityContainedOmnigentCatalogPath(cityPath, configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" || filepath.IsAbs(configured) {
		return "", fmt.Errorf("catalog path must be a non-empty city-relative path")
	}
	clean := filepath.Clean(configured)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("catalog path %q escapes the city", configured)
	}
	root, err := filepath.EvalSymlinks(cityPath)
	if err != nil {
		return "", fmt.Errorf("resolve city path: %w", err)
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("resolve catalog path: %w", err)
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", fmt.Errorf("compare catalog path to city: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("catalog path %q escapes the city", configured)
	}
	return candidate, nil
}

func omnigentCatalogResourceID(plan omnigent.AttachmentLaunchPlan) string {
	digest := strings.TrimPrefix(plan.CatalogSHA256, "sha256:")
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return "gco-catalog-" + plan.CapsuleKey.Token + "-" + digest
}
