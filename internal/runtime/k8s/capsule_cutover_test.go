package k8s

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProductionSeamPreservesCapsuleStateCapabilities(t *testing.T) {
	raw := newProviderWithOps(newFakeK8sOps())
	provider := newSeamBacked(raw)
	if _, ok := provider.(runtime.CapsuleStateRuntime); !ok {
		t.Fatal("production Kubernetes seam dropped CapsuleStateRuntime")
	}
	scoped, ok := provider.(runtime.CapsuleCityScopeProvider)
	if !ok {
		t.Fatal("production Kubernetes seam dropped CapsuleCityScopeProvider")
	}
	if got := scoped.CapsuleCityScope(); got != raw.capsuleCityScope {
		t.Fatalf("CapsuleCityScope = %q, want %q", got, raw.capsuleCityScope)
	}
}

func TestProductionSeamDerivesCapsuleCityScopeFromGasCityContext(t *testing.T) {
	raw := newProviderWithOps(newFakeK8sOps())
	raw.k8sContext = "kind-test"
	raw.namespace = "capsule-ns"
	provider, err := newSeamBackedForCity(raw, "capsule-city")
	if err != nil {
		t.Fatalf("newSeamBackedForCity: %v", err)
	}
	scoped := provider.(runtime.CapsuleCityScopeProvider)
	if got, want := scoped.CapsuleCityScope(), "kind-test/capsule-ns/capsule-city"; got != want {
		t.Fatalf("CapsuleCityScope = %q, want %q", got, want)
	}
}
