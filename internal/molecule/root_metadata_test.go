package molecule

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

var errAdmissionMetadataWrite = errors.New("injected admission metadata write failure")

type admissionObservingStore struct {
	*beads.MemStore
	expectedAdmission    string
	rejectRoot           bool
	createCalls          int
	rootAdmissionDurable bool
	routeBeforeAdmission bool
	routeMutations       int
}

type rejectingAdmissionGraphApplyStore struct {
	*beads.MemStore
	applyCalls int
}

func (s *rejectingAdmissionGraphApplyStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	s.applyCalls++
	for _, node := range plan.Nodes {
		if node.Metadata["admission"] == "rejected" {
			return nil, errAdmissionMetadataWrite
		}
	}
	return nil, errors.New("graph apply missing root admission metadata")
}

func (s *admissionObservingStore) Create(b beads.Bead) (beads.Bead, error) {
	s.createCalls++
	if s.createCalls == 1 {
		if got := b.Metadata["admission"]; got != s.expectedAdmission {
			return beads.Bead{}, errors.New("root create missing admission metadata")
		}
		if s.rejectRoot {
			return beads.Bead{}, errAdmissionMetadataWrite
		}
		created, err := s.MemStore.Create(b)
		if err == nil {
			s.rootAdmissionDurable = true
		}
		return created, err
	}
	if beadCanRoute(b) {
		s.routeMutations++
		if !s.rootAdmissionDurable {
			s.routeBeforeAdmission = true
		}
	}
	return s.MemStore.Create(b)
}

func (s *admissionObservingStore) Update(id string, opts beads.UpdateOpts) error {
	if updateCanRoute(opts) {
		s.routeMutations++
		if !s.rootAdmissionDurable {
			s.routeBeforeAdmission = true
		}
	}
	return s.MemStore.Update(id, opts)
}

func beadCanRoute(b beads.Bead) bool {
	return b.Assignee != "" ||
		!beads.IsReadyExcludedType(b.Type) ||
		hasActiveRoute(b.Metadata)
}

func updateCanRoute(opts beads.UpdateOpts) bool {
	return opts.Assignee != nil && *opts.Assignee != "" ||
		opts.Type != nil && !beads.IsReadyExcludedType(*opts.Type) ||
		hasActiveRoute(opts.Metadata)
}

func hasActiveRoute(metadata map[string]string) bool {
	return metadata[beadmeta.RunTargetMetadataKey] != "" ||
		metadata[beadmeta.RoutedToMetadataKey] != "" ||
		metadata[beadmeta.ExecutionRoutedToMetadataKey] != ""
}

func TestInstantiateRootMetadataIsDurableBeforeRouteActivation(t *testing.T) {
	setGraphApplyModeForTest(t, false)

	store := &admissionObservingStore{
		MemStore:          beads.NewMemStore(),
		expectedAdmission: "accepted",
	}
	result, err := Instantiate(context.Background(), store, admissionRecipe(), Options{
		IdempotencyKey: "generated-idempotency",
		RootMetadata: map[string]string{
			"admission": "accepted",
			"number":    "123",
			"boolean":   "true",
			"null":      "null",
			"array":     "[1,2]",
			"object":    `{"answer":42}`,
		},
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if store.routeBeforeAdmission {
		t.Fatal("a bead became routable before root admission metadata was durable")
	}
	if store.routeMutations == 0 {
		t.Fatal("test did not observe route activation")
	}

	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if got := root.Metadata["admission"]; got != "accepted" {
		t.Fatalf("root admission metadata = %q, want accepted", got)
	}
	if got := root.Metadata["idempotency_key"]; got != "generated-idempotency" {
		t.Fatalf("root idempotency metadata = %q, want protected generated value", got)
	}
	if got := root.Metadata[beadmeta.FormulaHashMetadataKey]; got != "compiled-hash" {
		t.Fatalf("root formula hash metadata = %q, want compiled provenance", got)
	}
	if got := root.Metadata[beadmeta.FormulaSourceMetadataKey]; got != "admission-workflow.formula.toml" {
		t.Fatalf("root formula source metadata = %q, want compiled provenance", got)
	}
	assertExactStringMetadata(t, root.Metadata)
}

func TestValidateRootMetadataRejectsEntireEngineNamespace(t *testing.T) {
	for _, key := range beadmeta.KnownMetadataKeys {
		key := key
		t.Run(key, func(t *testing.T) {
			err := ValidateRootMetadata(map[string]string{key: "attacker"})
			if err == nil {
				t.Fatalf("engine-owned root metadata key %q accepted", key)
			}
		})
	}

	for _, key := range []string{
		"idempotency_key",
		"workflow_id",
		beadmeta.MoleculeIDMetadataKey,
		beadmeta.MoleculeFailedMetadataKey,
		beadmeta.MergeStrategyMetadataKey,
		beadmeta.WorkerDirMetadataKey,
		beadmeta.ArtifactDirMetadataKey,
		beadmeta.LegacyWorkDirMetadataKey,
		beadmeta.FormulaVarPrefix + "convoy_id",
		beadmeta.OptionMetadataPrefix + "model",
		beadmeta.ControlDispatcherFallbackMetadataKey,
		beadmeta.FormulaHashMetadataKey,
		beadmeta.FormulaSourceMetadataKey,
		"gc.custom_annotation",
		"gc.future_unclassified_key",
		"gc.deferred_future_key",
		"gc.session_future_key",
		"gc.scope_future_key",
		"gc.source_future_key",
		"gc.graphv2_future_key",
	} {
		if err := ValidateRootMetadata(map[string]string{key: "attacker"}); err == nil {
			t.Errorf("reserved root metadata key %q accepted", key)
		}
	}

	if err := ValidateRootMetadata(map[string]string{
		"admission":                 "accepted",
		"spec.state":                "live",
		"speckit.pack_workspace":    "/workspace",
		"speckit.graph_vars.v1":     `{"feature":"factory"}`,
		"example.custom_annotation": "allowed",
	}); err != nil {
		t.Fatalf("caller-owned root annotation metadata rejected: %v", err)
	}
}

func TestValidateRootMetadataReportsProtectedKeysDeterministically(t *testing.T) {
	err := ValidateRootMetadata(map[string]string{
		beadmeta.RootBeadIDMetadataKey:         "attacker-root",
		beadmeta.AttachFencePendingMetadataKey: "false",
		"idempotency_key":                      "attacker-idempotency",
	})
	want := `root metadata contains protected engine-owned keys: "gc.attach_fence_pending", "gc.root_bead_id", "idempotency_key"`
	if err == nil || err.Error() != want {
		t.Fatalf("ValidateRootMetadata error = %q, want %q", err, want)
	}
}

func TestValidateExistingRootMetadataDistinguishesMissingFromExplicitEmpty(t *testing.T) {
	const key = "admission"

	err := ValidateExistingRootMetadata(beads.Bead{
		ID:       "root-missing",
		Metadata: map[string]string{},
	}, map[string]string{key: ""})
	if err == nil || !strings.Contains(err.Error(), `metadata mismatch for "admission": key is absent, want ""`) {
		t.Fatalf("missing-key match error = %v, want explicit absence mismatch", err)
	}

	err = ValidateExistingRootMetadata(beads.Bead{
		ID:       "root-empty",
		Metadata: map[string]string{key: ""},
	}, map[string]string{key: ""})
	if err != nil {
		t.Fatalf("explicit empty metadata did not match: %v", err)
	}
}

func TestInstantiateRejectsProtectedRootMetadataBeforeStoreMutation(t *testing.T) {
	setGraphApplyModeForTest(t, false)

	store := &admissionObservingStore{MemStore: beads.NewMemStore()}
	_, err := Instantiate(context.Background(), store, admissionRecipe(), Options{
		RootMetadata: map[string]string{
			"idempotency_key":                 "attacker-idempotency",
			beadmeta.FormulaHashMetadataKey:   "attacker-hash",
			beadmeta.FormulaSourceMetadataKey: "attacker-source",
			"gc.future_engine_key":            "attacker-future",
		},
	})
	if err == nil {
		t.Fatal("Instantiate accepted protected engine metadata")
	}
	if store.createCalls != 0 {
		t.Fatalf("protected metadata reached Create %d time(s), want zero", store.createCalls)
	}
	items, listErr := store.ListOpen()
	if listErr != nil {
		t.Fatalf("ListOpen: %v", listErr)
	}
	if len(items) != 0 {
		t.Fatalf("protected metadata left %d bead(s), want none: %#v", len(items), items)
	}
}

func TestInstantiateRootMetadataWriteFailureLeavesNoGraph(t *testing.T) {
	setGraphApplyModeForTest(t, false)

	store := &admissionObservingStore{
		MemStore:          beads.NewMemStore(),
		expectedAdmission: "rejected",
		rejectRoot:        true,
	}
	_, err := Instantiate(context.Background(), store, admissionRecipe(), Options{
		RootMetadata: map[string]string{"admission": "rejected"},
	})
	if !errors.Is(err, errAdmissionMetadataWrite) {
		t.Fatalf("Instantiate error = %v, want injected admission metadata failure", err)
	}
	if store.createCalls != 1 {
		t.Fatalf("Create calls = %d, want only rejected root create", store.createCalls)
	}
	items, listErr := store.ListOpen()
	if listErr != nil {
		t.Fatalf("ListOpen: %v", listErr)
	}
	if len(items) != 0 {
		t.Fatalf("metadata failure left %d bead(s), want none: %#v", len(items), items)
	}
	if store.routeBeforeAdmission || store.routeMutations != 0 {
		t.Fatalf("metadata failure exposed route mutations: before_admission=%v mutations=%d", store.routeBeforeAdmission, store.routeMutations)
	}
}

func TestGraphApplyRootMetadataWriteFailureLeavesNoGraph(t *testing.T) {
	setGraphApplyModeForTest(t, true)

	store := &rejectingAdmissionGraphApplyStore{MemStore: beads.NewMemStore()}
	_, err := Instantiate(context.Background(), store, admissionRecipe(), Options{
		RootMetadata: map[string]string{"admission": "rejected"},
	})
	if !errors.Is(err, errAdmissionMetadataWrite) {
		t.Fatalf("Instantiate error = %v, want injected graph-apply metadata failure", err)
	}
	if store.applyCalls != 1 {
		t.Fatalf("ApplyGraphPlan calls = %d, want one rejected atomic apply", store.applyCalls)
	}
	items, listErr := store.ListOpen()
	if listErr != nil {
		t.Fatalf("ListOpen: %v", listErr)
	}
	if len(items) != 0 {
		t.Fatalf("graph-apply metadata failure left %d bead(s), want none: %#v", len(items), items)
	}
}

func TestBuildRecipeApplyPlanIncludesRootMetadataBeforeAtomicApply(t *testing.T) {
	plan, _, rootKey, err := buildRecipeApplyPlan(admissionRecipe(), Options{
		IdempotencyKey: "generated-idempotency",
		RootMetadata: map[string]string{
			"admission": "accepted",
			"number":    "123",
			"boolean":   "true",
			"null":      "null",
			"array":     "[1,2]",
			"object":    `{"answer":42}`,
		},
	})
	if err != nil {
		t.Fatalf("buildRecipeApplyPlan: %v", err)
	}
	var root *beads.GraphApplyNode
	for i := range plan.Nodes {
		if plan.Nodes[i].Key == rootKey {
			root = &plan.Nodes[i]
			break
		}
	}
	if root == nil {
		t.Fatalf("plan has no root node %q", rootKey)
	}
	if got := root.Metadata["admission"]; got != "accepted" {
		t.Fatalf("root admission metadata = %q, want accepted", got)
	}
	if got := root.Metadata["idempotency_key"]; got != "generated-idempotency" {
		t.Fatalf("root idempotency metadata = %q, want protected generated value", got)
	}
	if got := root.Metadata[beadmeta.FormulaHashMetadataKey]; got != "compiled-hash" {
		t.Fatalf("root formula hash metadata = %q, want compiled provenance", got)
	}
	if got := root.Metadata[beadmeta.FormulaSourceMetadataKey]; got != "admission-workflow.formula.toml" {
		t.Fatalf("root formula source metadata = %q, want compiled provenance", got)
	}
	assertExactStringMetadata(t, root.Metadata)
}

func TestBuildRecipeApplyPlanRejectsProtectedRootMetadata(t *testing.T) {
	_, _, _, err := buildRecipeApplyPlan(admissionRecipe(), Options{
		RootMetadata: map[string]string{beadmeta.KindMetadataKey: "attacker-kind"},
	})
	if err == nil {
		t.Fatal("buildRecipeApplyPlan accepted protected root kind metadata")
	}
}

func assertExactStringMetadata(t *testing.T, metadata map[string]string) {
	t.Helper()
	want := map[string]string{
		"number":  "123",
		"boolean": "true",
		"null":    "null",
		"array":   "[1,2]",
		"object":  `{"answer":42}`,
	}
	for key, value := range want {
		if got := metadata[key]; got != value {
			t.Errorf("root metadata[%q] = %q, want exact string %q", key, got, value)
		}
	}
}

func admissionRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name:          "admission-workflow",
		ContentHash:   "compiled-hash",
		FormulaSource: "admission-workflow.formula.toml",
		Steps: []formula.RecipeStep{
			{
				ID:     "admission-workflow",
				Title:  "Admission workflow",
				Type:   "task",
				IsRoot: true,
				Metadata: map[string]string{
					beadmeta.KindMetadataKey: "workflow",
					"admission":              "formula-default",
				},
			},
			{
				ID:       "admission-workflow.work",
				Title:    "Do admitted work",
				Type:     "task",
				Assignee: "worker",
				Metadata: map[string]string{
					beadmeta.RoutedToMetadataKey: "rig/worker",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "admission-workflow.work", DependsOnID: "admission-workflow", Type: "parent-child"},
		},
	}
}
