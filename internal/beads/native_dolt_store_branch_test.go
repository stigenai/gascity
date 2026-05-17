package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// nativeDoltBranchSpy satisfies beadslib.Storage for the branch-coverage tests.
// It embeds beadslib.Storage so unimplemented methods panic on unexpected calls.
type nativeDoltBranchSpy struct {
	beadslib.Storage
	getIssue                    func(context.Context, string) (*beadslib.Issue, error)
	updateIssue                 func(context.Context, string, map[string]interface{}, string) error
	closeIssue                  func(context.Context, string, string, string, string) error
	addLabel                    func(context.Context, string, string, string) error
	removeLabel                 func(context.Context, string, string, string) error
	getReadyWork                func(context.Context, beadslib.WorkFilter) ([]*beadslib.Issue, error)
	addDependency               func(context.Context, *beadslib.Dependency, string) error
	removeDependency            func(context.Context, string, string, string) error
	getDependenciesWithMetadata func(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error)
	getDependentsWithMetadata   func(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error)
}

func (s *nativeDoltBranchSpy) GetIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	return s.getIssue(ctx, id)
}

func (s *nativeDoltBranchSpy) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return s.updateIssue(ctx, id, updates, actor)
}

func (s *nativeDoltBranchSpy) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	return s.closeIssue(ctx, id, reason, actor, session)
}

func (s *nativeDoltBranchSpy) AddLabel(ctx context.Context, id, label, actor string) error {
	return s.addLabel(ctx, id, label, actor)
}

func (s *nativeDoltBranchSpy) RemoveLabel(ctx context.Context, id, label, actor string) error {
	return s.removeLabel(ctx, id, label, actor)
}

func (s *nativeDoltBranchSpy) GetReadyWork(ctx context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
	return s.getReadyWork(ctx, filter)
}

func (s *nativeDoltBranchSpy) AddDependency(ctx context.Context, dep *beadslib.Dependency, actor string) error {
	return s.addDependency(ctx, dep, actor)
}

func (s *nativeDoltBranchSpy) RemoveDependency(ctx context.Context, issueID, dependsOnID, actor string) error {
	return s.removeDependency(ctx, issueID, dependsOnID, actor)
}

func (s *nativeDoltBranchSpy) GetDependenciesWithMetadata(ctx context.Context, id string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	return s.getDependenciesWithMetadata(ctx, id)
}

func (s *nativeDoltBranchSpy) GetDependentsWithMetadata(ctx context.Context, id string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	return s.getDependentsWithMetadata(ctx, id)
}

// --- Update: field updates ---

func TestNativeDoltStoreUpdateDelegatesFieldsToUpstream(t *testing.T) {
	title := "updated title"
	status := "in_progress"
	priority := 1
	assignee := "gascity/builder"

	var capturedID string
	var capturedUpdates map[string]interface{}
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id}, nil
		},
		updateIssue: func(_ context.Context, id string, updates map[string]interface{}, _ string) error {
			capturedID = id
			capturedUpdates = updates
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Update("gc-u1", UpdateOpts{
		Title:    &title,
		Status:   &status,
		Priority: &priority,
		Assignee: &assignee,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if capturedID != "gc-u1" {
		t.Fatalf("UpdateIssue id = %q, want gc-u1", capturedID)
	}
	if capturedUpdates["title"] != title {
		t.Fatalf("title = %v, want %q", capturedUpdates["title"], title)
	}
	if capturedUpdates["status"] != status {
		t.Fatalf("status = %v, want %q", capturedUpdates["status"], status)
	}
	if capturedUpdates["priority"] != priority {
		t.Fatalf("priority = %v, want %d", capturedUpdates["priority"], priority)
	}
	if capturedUpdates["assignee"] != assignee {
		t.Fatalf("assignee = %v, want %q", capturedUpdates["assignee"], assignee)
	}
}

func TestNativeDoltStoreUpdateMergesMetadataWithExisting(t *testing.T) {
	existingMeta := json.RawMessage(`{"gc.existing":"keep","gc.key":"old"}`)
	var capturedUpdates map[string]interface{}
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id, Metadata: existingMeta}, nil
		},
		updateIssue: func(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
			capturedUpdates = updates
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Update("gc-u2", UpdateOpts{
		Metadata: map[string]string{"gc.key": "new", "gc.added": "yes"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	raw, ok := capturedUpdates["metadata"].(json.RawMessage)
	if !ok {
		t.Fatalf("metadata in updates is not json.RawMessage: %T", capturedUpdates["metadata"])
	}
	var merged map[string]string
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("unmarshal merged metadata: %v", err)
	}
	if merged["gc.existing"] != "keep" {
		t.Fatalf("gc.existing = %q, want keep (existing key must be preserved)", merged["gc.existing"])
	}
	if merged["gc.key"] != "new" {
		t.Fatalf("gc.key = %q, want new (incoming value overwrites old)", merged["gc.key"])
	}
	if merged["gc.added"] != "yes" {
		t.Fatalf("gc.added = %q, want yes (new key added)", merged["gc.added"])
	}
}

func TestNativeDoltStoreUpdateMetadataMergeGetIssueFailurePropagated(t *testing.T) {
	wantErr := errors.New("get failed during metadata merge")
	storage := &nativeDoltBranchSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return nil, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Update("gc-u3", UpdateOpts{
		Metadata: map[string]string{"gc.k": "v"},
	}); !errors.Is(err, wantErr) {
		t.Fatalf("Update error = %v, want %v", err, wantErr)
	}
}

// --- Update: label add/remove ---

func TestNativeDoltStoreUpdateAddsLabels(t *testing.T) {
	var addedLabels []string
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			return nil
		},
		addLabel: func(_ context.Context, _, label, _ string) error {
			addedLabels = append(addedLabels, label)
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Update("gc-u4", UpdateOpts{Labels: []string{"needs-tests", "ready-to-build"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(addedLabels) != 2 || addedLabels[0] != "needs-tests" || addedLabels[1] != "ready-to-build" {
		t.Fatalf("addedLabels = %v, want [needs-tests ready-to-build]", addedLabels)
	}
}

func TestNativeDoltStoreUpdateRemovesLabels(t *testing.T) {
	var removedLabels []string
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			return nil
		},
		removeLabel: func(_ context.Context, _, label, _ string) error {
			removedLabels = append(removedLabels, label)
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Update("gc-u5", UpdateOpts{RemoveLabels: []string{"needs-tests"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(removedLabels) != 1 || removedLabels[0] != "needs-tests" {
		t.Fatalf("removedLabels = %v, want [needs-tests]", removedLabels)
	}
}

func TestNativeDoltStoreUpdateAddLabelErrorPropagated(t *testing.T) {
	wantErr := errors.New("add label failed")
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			return nil
		},
		addLabel: func(context.Context, string, string, string) error {
			return wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Update("gc-u6", UpdateOpts{Labels: []string{"any"}}); !errors.Is(err, wantErr) {
		t.Fatalf("Update AddLabel error = %v, want %v", err, wantErr)
	}
}

// --- Update: ParentID reparent/clear ---

func TestNativeDoltStoreUpdateReparentsToNewParent(t *testing.T) {
	var removedFrom, removedTo string
	var addedDep *beadslib.Dependency
	newParent := "gc-parent-new"
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			return nil
		},
		getDependenciesWithMetadata: func(_ context.Context, id string) ([]*beadslib.IssueWithDependencyMetadata, error) {
			return []*beadslib.IssueWithDependencyMetadata{
				{Issue: beadslib.Issue{ID: "gc-parent-old"}, DependencyType: beadslib.DepParentChild},
			}, nil
		},
		removeDependency: func(_ context.Context, from, to, _ string) error {
			removedFrom = from
			removedTo = to
			return nil
		},
		addDependency: func(_ context.Context, dep *beadslib.Dependency, _ string) error {
			addedDep = dep
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Update("gc-child", UpdateOpts{ParentID: &newParent}); err != nil {
		t.Fatalf("Update reparent: %v", err)
	}

	if removedFrom != "gc-child" || removedTo != "gc-parent-old" {
		t.Fatalf("RemoveDependency args = (%q, %q), want (gc-child, gc-parent-old)", removedFrom, removedTo)
	}
	if addedDep == nil || addedDep.IssueID != "gc-child" || addedDep.DependsOnID != newParent || addedDep.Type != beadslib.DepParentChild {
		t.Fatalf("AddDependency dep = %#v, want parent-child gc-child→gc-parent-new", addedDep)
	}
}

func TestNativeDoltStoreUpdateClearsParentWithEmptyString(t *testing.T) {
	addDepCalled := false
	emptyParent := ""
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			return nil
		},
		getDependenciesWithMetadata: func(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error) {
			return []*beadslib.IssueWithDependencyMetadata{
				{Issue: beadslib.Issue{ID: "gc-old-parent"}, DependencyType: beadslib.DepParentChild},
			}, nil
		},
		removeDependency: func(context.Context, string, string, string) error { return nil },
		addDependency: func(context.Context, *beadslib.Dependency, string) error {
			addDepCalled = true
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Update("gc-child", UpdateOpts{ParentID: &emptyParent}); err != nil {
		t.Fatalf("Update clear parent: %v", err)
	}
	if addDepCalled {
		t.Fatal("AddDependency should not be called when clearing parent with empty string")
	}
}

// --- Close ---

func TestNativeDoltStoreCloseIsNoOpWhenAlreadyClosed(t *testing.T) {
	closeCalled := false
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id, Status: beadslib.StatusClosed}, nil
		},
		closeIssue: func(context.Context, string, string, string, string) error {
			closeCalled = true
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Close("gc-already-closed"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closeCalled {
		t.Fatal("CloseIssue must not be called when bead is already closed")
	}
}

func TestNativeDoltStoreCloseDelegatesToUpstream(t *testing.T) {
	var closedID string
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id, Status: beadslib.StatusOpen}, nil
		},
		closeIssue: func(_ context.Context, id, _, _, _ string) error {
			closedID = id
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Close("gc-open"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closedID != "gc-open" {
		t.Fatalf("CloseIssue id = %q, want gc-open", closedID)
	}
}

func TestNativeDoltStoreCloseGetIssueFailurePropagated(t *testing.T) {
	wantErr := errors.New("get failed before close")
	storage := &nativeDoltBranchSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return nil, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Close("gc-any"); !errors.Is(err, wantErr) {
		t.Fatalf("Close error = %v, want %v", err, wantErr)
	}
}

func TestNativeDoltStoreCloseIssueFailurePropagated(t *testing.T) {
	wantErr := errors.New("close issue failed")
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id, Status: beadslib.StatusOpen}, nil
		},
		closeIssue: func(context.Context, string, string, string, string) error {
			return wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Close("gc-open"); !errors.Is(err, wantErr) {
		t.Fatalf("Close error = %v, want %v", err, wantErr)
	}
}

// --- Ready ---

func TestNativeDoltStoreReadyPropagatesAssigneeToWorkFilter(t *testing.T) {
	var capturedFilter beadslib.WorkFilter
	storage := &nativeDoltBranchSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			capturedFilter = filter
			return nil, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Ready(ReadyQuery{Assignee: "gascity/builder"}); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if capturedFilter.Assignee == nil || *capturedFilter.Assignee != "gascity/builder" {
		t.Fatalf("WorkFilter.Assignee = %v, want &gascity/builder", capturedFilter.Assignee)
	}
}

func TestNativeDoltStoreReadyPropagatesLimitToWorkFilter(t *testing.T) {
	var capturedFilter beadslib.WorkFilter
	storage := &nativeDoltBranchSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			capturedFilter = filter
			return nil, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Ready(ReadyQuery{Limit: 5}); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if capturedFilter.Limit != 5 {
		t.Fatalf("WorkFilter.Limit = %d, want 5", capturedFilter.Limit)
	}
}

func TestNativeDoltStoreReadyNoArgsOmitsAssigneeFilter(t *testing.T) {
	var capturedFilter beadslib.WorkFilter
	storage := &nativeDoltBranchSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			capturedFilter = filter
			return nil, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if capturedFilter.Assignee != nil {
		t.Fatalf("WorkFilter.Assignee = %v, want nil when no assignee specified", capturedFilter.Assignee)
	}
}

func TestNativeDoltStoreReadyUpstreamFailurePropagated(t *testing.T) {
	wantErr := errors.New("ready work failed")
	storage := &nativeDoltBranchSpy{
		getReadyWork: func(context.Context, beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			return nil, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Ready(); !errors.Is(err, wantErr) {
		t.Fatalf("Ready error = %v, want %v", err, wantErr)
	}
}

// --- SetMetadataBatch ---

func TestNativeDoltStoreSetMetadataBatchMergesWithExisting(t *testing.T) {
	existing := json.RawMessage(`{"gc.a":"old","gc.b":"keep"}`)
	var capturedUpdates map[string]interface{}
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id, Metadata: existing}, nil
		},
		updateIssue: func(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
			capturedUpdates = updates
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.SetMetadataBatch("gc-m1", map[string]string{"gc.a": "new", "gc.c": "added"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	raw, ok := capturedUpdates["metadata"].(json.RawMessage)
	if !ok {
		t.Fatalf("metadata in updates is not json.RawMessage: %T", capturedUpdates["metadata"])
	}
	var result map[string]string
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["gc.a"] != "new" {
		t.Fatalf("gc.a = %q, want new", result["gc.a"])
	}
	if result["gc.b"] != "keep" {
		t.Fatalf("gc.b = %q, want keep (must preserve existing)", result["gc.b"])
	}
	if result["gc.c"] != "added" {
		t.Fatalf("gc.c = %q, want added", result["gc.c"])
	}
}

func TestNativeDoltStoreSetMetadataBatchGetIssueFailurePropagated(t *testing.T) {
	wantErr := errors.New("get failed in SetMetadataBatch")
	storage := &nativeDoltBranchSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return nil, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.SetMetadataBatch("gc-m2", map[string]string{"k": "v"}); !errors.Is(err, wantErr) {
		t.Fatalf("SetMetadataBatch GetIssue error = %v, want %v", err, wantErr)
	}
}

func TestNativeDoltStoreSetMetadataBatchUpdateIssueFailurePropagated(t *testing.T) {
	wantErr := errors.New("update failed in SetMetadataBatch")
	storage := &nativeDoltBranchSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			return wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.SetMetadataBatch("gc-m3", map[string]string{"k": "v"}); !errors.Is(err, wantErr) {
		t.Fatalf("SetMetadataBatch UpdateIssue error = %v, want %v", err, wantErr)
	}
}

// --- DepAdd ---

func TestNativeDoltStoreDepAddDelegatesToUpstream(t *testing.T) {
	var capturedDep *beadslib.Dependency
	storage := &nativeDoltBranchSpy{
		addDependency: func(_ context.Context, dep *beadslib.Dependency, _ string) error {
			capturedDep = dep
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.DepAdd("gc-child", "gc-parent", "parent-child"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	if capturedDep == nil {
		t.Fatal("AddDependency was not called")
	}
	if capturedDep.IssueID != "gc-child" || capturedDep.DependsOnID != "gc-parent" {
		t.Fatalf("Dependency = %#v, want gc-child→gc-parent", capturedDep)
	}
	if capturedDep.Type != beadslib.DependencyType("parent-child") {
		t.Fatalf("Dependency.Type = %q, want parent-child", capturedDep.Type)
	}
}

func TestNativeDoltStoreDepAddErrorPropagated(t *testing.T) {
	wantErr := errors.New("add dep failed")
	storage := &nativeDoltBranchSpy{
		addDependency: func(context.Context, *beadslib.Dependency, string) error {
			return wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.DepAdd("gc-a", "gc-b", "blocks"); !errors.Is(err, wantErr) {
		t.Fatalf("DepAdd error = %v, want %v", err, wantErr)
	}
}

// --- DepRemove ---

func TestNativeDoltStoreDepRemoveDelegatesToUpstream(t *testing.T) {
	var removedFrom, removedTo string
	storage := &nativeDoltBranchSpy{
		removeDependency: func(_ context.Context, from, to, _ string) error {
			removedFrom = from
			removedTo = to
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.DepRemove("gc-a", "gc-b"); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}
	if removedFrom != "gc-a" || removedTo != "gc-b" {
		t.Fatalf("RemoveDependency args = (%q, %q), want (gc-a, gc-b)", removedFrom, removedTo)
	}
}

func TestNativeDoltStoreDepRemoveErrorPropagated(t *testing.T) {
	wantErr := errors.New("remove dep failed")
	storage := &nativeDoltBranchSpy{
		removeDependency: func(context.Context, string, string, string) error {
			return wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.DepRemove("gc-a", "gc-b"); !errors.Is(err, wantErr) {
		t.Fatalf("DepRemove error = %v, want %v", err, wantErr)
	}
}

// --- DepList "down" (dependencies) ---

func TestNativeDoltStoreDepListDownConvertsDependencies(t *testing.T) {
	storage := &nativeDoltBranchSpy{
		getDependenciesWithMetadata: func(_ context.Context, id string) ([]*beadslib.IssueWithDependencyMetadata, error) {
			return []*beadslib.IssueWithDependencyMetadata{
				{Issue: beadslib.Issue{ID: "gc-dep1"}, DependencyType: beadslib.DepBlocks},
				{Issue: beadslib.Issue{ID: "gc-dep2"}, DependencyType: beadslib.DepParentChild},
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	deps, err := store.DepList("gc-root", "down")
	if err != nil {
		t.Fatalf("DepList down: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("deps len = %d, want 2", len(deps))
	}
	for _, d := range deps {
		if d.IssueID != "gc-root" {
			t.Fatalf("dep.IssueID = %q, want gc-root for down direction", d.IssueID)
		}
	}
	if deps[0].DependsOnID != "gc-dep1" || string(deps[0].Type) != string(beadslib.DepBlocks) {
		t.Fatalf("dep[0] = %#v, want gc-root→gc-dep1 blocks", deps[0])
	}
	if deps[1].DependsOnID != "gc-dep2" || string(deps[1].Type) != string(beadslib.DepParentChild) {
		t.Fatalf("dep[1] = %#v, want gc-root→gc-dep2 parent-child", deps[1])
	}
}

func TestNativeDoltStoreDepListDownErrorPropagated(t *testing.T) {
	wantErr := errors.New("get deps failed")
	storage := &nativeDoltBranchSpy{
		getDependenciesWithMetadata: func(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error) {
			return nil, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.DepList("gc-root", "down"); !errors.Is(err, wantErr) {
		t.Fatalf("DepList down error = %v, want %v", err, wantErr)
	}
}

// --- DepList "up" (dependents) ---

func TestNativeDoltStoreDepListUpConvertsDependents(t *testing.T) {
	storage := &nativeDoltBranchSpy{
		getDependentsWithMetadata: func(_ context.Context, id string) ([]*beadslib.IssueWithDependencyMetadata, error) {
			return []*beadslib.IssueWithDependencyMetadata{
				{Issue: beadslib.Issue{ID: "gc-dep1"}, DependencyType: beadslib.DepBlocks},
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	deps, err := store.DepList("gc-root", "up")
	if err != nil {
		t.Fatalf("DepList up: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("deps len = %d, want 1", len(deps))
	}
	if deps[0].IssueID != "gc-dep1" || deps[0].DependsOnID != "gc-root" {
		t.Fatalf("dep = %#v, want IssueID=gc-dep1, DependsOnID=gc-root for up direction", deps[0])
	}
}

func TestNativeDoltStoreDepListUpErrorPropagated(t *testing.T) {
	wantErr := errors.New("get dependents failed")
	storage := &nativeDoltBranchSpy{
		getDependentsWithMetadata: func(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error) {
			return nil, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.DepList("gc-root", "up"); !errors.Is(err, wantErr) {
		t.Fatalf("DepList up error = %v, want %v", err, wantErr)
	}
}

// --- nativeIssueFilterFromListQuery ---

func TestNativeIssueFilterTierWispsSetEphemeralTrue(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{TierMode: TierWisps})
	if f.Ephemeral == nil || !*f.Ephemeral {
		t.Fatalf("Ephemeral = %v, want &true for TierWisps", f.Ephemeral)
	}
}

func TestNativeIssueFilterTierBothOmitsEphemeralFilter(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{TierMode: TierBoth})
	if f.Ephemeral != nil {
		t.Fatalf("Ephemeral = %v, want nil for TierBoth", f.Ephemeral)
	}
}

func TestNativeIssueFilterDefaultTierSetEphemeralFalse(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{})
	if f.Ephemeral == nil || *f.Ephemeral {
		t.Fatalf("Ephemeral = %v, want &false for default tier", f.Ephemeral)
	}
}

func TestNativeIssueFilterStatusSetSetsStatusFilter(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{Status: "in_progress"})
	if f.Status == nil || string(*f.Status) != "in_progress" {
		t.Fatalf("Status = %v, want &in_progress", f.Status)
	}
	if len(f.ExcludeStatus) != 0 {
		t.Fatalf("ExcludeStatus = %v, want empty when Status is set", f.ExcludeStatus)
	}
}

func TestNativeIssueFilterNoStatusExcludesClosedByDefault(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{IncludeClosed: false})
	if len(f.ExcludeStatus) != 1 || string(f.ExcludeStatus[0]) != "closed" {
		t.Fatalf("ExcludeStatus = %v, want [closed] when no status + IncludeClosed=false", f.ExcludeStatus)
	}
	if f.Status != nil {
		t.Fatalf("Status = %v, want nil", f.Status)
	}
}

func TestNativeIssueFilterIncludeClosedOmitsExcludeStatus(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{IncludeClosed: true})
	if f.Status != nil {
		t.Fatalf("Status = %v, want nil", f.Status)
	}
	if len(f.ExcludeStatus) != 0 {
		t.Fatalf("ExcludeStatus = %v, want empty when IncludeClosed=true", f.ExcludeStatus)
	}
}

func TestNativeIssueFilterTypePropagated(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{Type: "message"})
	if f.IssueType == nil || string(*f.IssueType) != "message" {
		t.Fatalf("IssueType = %v, want &message", f.IssueType)
	}
}

func TestNativeIssueFilterLabelPropagated(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{Label: "needs-tests"})
	if len(f.Labels) != 1 || f.Labels[0] != "needs-tests" {
		t.Fatalf("Labels = %v, want [needs-tests]", f.Labels)
	}
}

func TestNativeIssueFilterAssigneePropagated(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{Assignee: "gascity/builder"})
	if f.Assignee == nil || *f.Assignee != "gascity/builder" {
		t.Fatalf("Assignee = %v, want &gascity/builder", f.Assignee)
	}
}

func TestNativeIssueFilterParentIDPropagated(t *testing.T) {
	f := nativeIssueFilterFromListQuery(ListQuery{ParentID: "gc-mol-root"})
	if f.ParentID == nil || *f.ParentID != "gc-mol-root" {
		t.Fatalf("ParentID = %v, want &gc-mol-root", f.ParentID)
	}
}
