package hybrid

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func isRemote(name string) bool { return strings.Contains(name, "remote-agent") }

type lifecycleProvider struct {
	*runtime.Fake
	configureCalls int
	teardownCalls  int
}

type freshListProvider struct {
	*runtime.Fake
	freshNames []string
	freshCalls int
}

type noFencedStopProvider struct{ runtime.Provider }

func TestProvider_ResolvesFencedStopPerRoute(t *testing.T) {
	local := noFencedStopProvider{Provider: runtime.NewFake()}
	remote := runtime.NewFake()
	h := New(local, remote, isRemote)

	if fenced, ok := runtime.ResolveInstanceTokenFencedStop(h, "local-agent"); ok || fenced != nil {
		t.Fatalf("local fenced-stop resolution = (%T,%t), want unavailable", fenced, ok)
	}
	fenced, ok := runtime.ResolveInstanceTokenFencedStop(h, "remote-agent-1")
	if !ok || fenced == nil {
		t.Fatalf("remote fenced-stop resolution = (%T,%t), want routed atomic provider", fenced, ok)
	}
	if err := h.StopIfInstanceToken("local-agent", "tok"); !errors.Is(err, runtime.ErrFencedStopUnsupported) {
		t.Fatalf("local StopIfInstanceToken error = %v, want ErrFencedStopUnsupported", err)
	}
}

// TestIsRunningChecked_RoutesAndPropagatesInconclusive covers gcy-dyl: a
// hybrid-wrapped k8s session must keep IsRunningChecked's
// confirmed-vs-inconclusive distinction instead of this router silently
// collapsing it back to a bare bool. Both the correct-backend routing and
// the error propagation are load-bearing — a wrong backend or a swallowed
// error would both pass a weaker test.
func TestIsRunningChecked_RoutesAndPropagatesInconclusive(t *testing.T) {
	local := runtime.NewFake()
	remote := runtime.NewFake()
	h := New(local, remote, isRemote)

	if running, err := h.IsRunningChecked("local-agent"); running || err != nil {
		t.Fatalf("IsRunningChecked(local-agent) = (%v,%v), want confirmed-not-running", running, err)
	}
	local.IsRunningErrors["local-agent"] = errors.New("simulated local timeout")
	if _, err := h.IsRunningChecked("local-agent"); err == nil {
		t.Fatal("IsRunningChecked(local-agent) swallowed local's inconclusive probe")
	}
	// The remote backend's own error must not leak onto the local route.
	remote.IsRunningErrors["remote-agent-1"] = errors.New("simulated remote timeout")
	if _, err := h.IsRunningChecked("local-agent"); err == nil {
		t.Fatal("IsRunningChecked(local-agent) should still be inconclusive from local's own error, not masked by remote's")
	}

	if _, err := h.IsRunningChecked("remote-agent-1"); err == nil {
		t.Fatal("IsRunningChecked(remote-agent-1) should route to remote and surface remote's inconclusive probe")
	}
}

// TestIsAttachedChecked_RoutesPerBackend covers gcy-dyl for IsAttachedChecked.
func TestIsAttachedChecked_RoutesPerBackend(t *testing.T) {
	local := runtime.NewFake()
	remote := runtime.NewFake()
	h := New(local, remote, isRemote)

	remote.IsAttachedErrors["remote-agent-1"] = errors.New("simulated remote timeout")
	if _, err := h.IsAttachedChecked("remote-agent-1"); err == nil {
		t.Fatal("IsAttachedChecked(remote-agent-1) should route to remote and surface its inconclusive probe")
	}
	if _, err := h.IsAttachedChecked("local-agent"); err != nil {
		t.Fatalf("IsAttachedChecked(local-agent) = err %v, want nil (routes to local, unaffected by remote's error)", err)
	}
}

// TestProcessAliveChecked_RoutesPerBackend covers gcy-dyl for
// ProcessAliveChecked.
func TestProcessAliveChecked_RoutesPerBackend(t *testing.T) {
	local := runtime.NewFake()
	remote := runtime.NewFake()
	h := New(local, remote, isRemote)

	remote.ProcessAliveErrors["remote-agent-1"] = errors.New("simulated remote timeout")
	if _, err := h.ProcessAliveChecked("remote-agent-1", []string{"claude"}); err == nil {
		t.Fatal("ProcessAliveChecked(remote-agent-1) should route to remote and surface its inconclusive probe")
	}
	if _, err := h.ProcessAliveChecked("local-agent", []string{"claude"}); err != nil {
		t.Fatalf("ProcessAliveChecked(local-agent) = err %v, want nil (routes to local, unaffected by remote's error)", err)
	}
}

func (p *freshListProvider) ListRunningFresh(prefix string) ([]string, error) {
	p.freshCalls++
	var names []string
	for _, name := range p.freshNames {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

func (p *lifecycleProvider) ConfigureServer() error {
	p.configureCalls++
	return nil
}

func (p *lifecycleProvider) TeardownServer() error {
	p.teardownCalls++
	return nil
}

func TestProvider_ForwardsLocalServerLifecycle(t *testing.T) {
	local := &lifecycleProvider{Fake: runtime.NewFake()}
	remote := &lifecycleProvider{Fake: runtime.NewFake()}
	h := New(local, remote, isRemote)

	if err := h.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}
	if err := h.TeardownServer(); err != nil {
		t.Fatalf("TeardownServer: %v", err)
	}
	if local.configureCalls != 1 || local.teardownCalls != 1 {
		t.Fatalf(
			"local lifecycle calls = configure:%d teardown:%d, want 1 each",
			local.configureCalls,
			local.teardownCalls,
		)
	}
	if remote.configureCalls != 0 || remote.teardownCalls != 0 {
		t.Fatalf(
			"remote lifecycle calls = configure:%d teardown:%d, want 0 each",
			remote.configureCalls,
			remote.teardownCalls,
		)
	}
}

// Relaunch must reach the routed backend (local vs remote), or the reconciler's
// RelaunchProvider type-assert would be masked by the hybrid router and fall
// back to Stop+Start.
func TestProvider_ForwardsRelaunchToRoutedBackend(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)
	if err := local.Start(context.Background(), "local-agent", runtime.Config{Command: "c"}); err != nil {
		t.Fatalf("Start(local): %v", err)
	}
	if err := remote.Start(context.Background(), "remote-agent-1", runtime.Config{Command: "c"}); err != nil {
		t.Fatalf("Start(remote): %v", err)
	}

	if err := h.Relaunch(context.Background(), "local-agent", runtime.Config{Command: "c2"}); err != nil {
		t.Fatalf("Relaunch(local): %v", err)
	}
	if got := local.CountCalls("Relaunch", "local-agent"); got != 1 {
		t.Errorf("local backend Relaunch calls = %d, want 1", got)
	}
	if err := h.Relaunch(context.Background(), "remote-agent-1", runtime.Config{Command: "c2"}); err != nil {
		t.Fatalf("Relaunch(remote): %v", err)
	}
	if got := remote.CountCalls("Relaunch", "remote-agent-1"); got != 1 {
		t.Errorf("remote backend Relaunch calls = %d, want 1", got)
	}
}

func TestStart_RoutesToLocal(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	if err := h.Start(context.Background(), "local-agent", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if !local.IsRunning("local-agent") {
		t.Error("expected local to have session")
	}
	if remote.IsRunning("local-agent") {
		t.Error("remote should not have session")
	}
}

func TestStart_RoutesToRemote(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	if err := h.Start(context.Background(), "remote-agent-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if local.IsRunning("remote-agent-1") {
		t.Error("local should not have session")
	}
	if !remote.IsRunning("remote-agent-1") {
		t.Error("expected remote to have session")
	}
}

func TestListRunning_MergesBothBackends(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	_ = h.Start(context.Background(), "gc-demo--local-agent", runtime.Config{})
	_ = h.Start(context.Background(), "gc-demo--remote-agent-1", runtime.Config{})
	_ = h.Start(context.Background(), "gc-demo--remote-agent-2", runtime.Config{})

	names, err := h.ListRunning("gc-demo-")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 sessions, got %d: %v", len(names), names)
	}
}

func TestListRunningFreshPreservesRemoteFreshInventory(t *testing.T) {
	local := runtime.NewFake()
	remote := &freshListProvider{
		Fake:       runtime.NewFake(),
		freshNames: []string{"gc-demo--remote-agent-late"},
	}
	h := New(local, remote, isRemote)
	if err := local.Start(context.Background(), "gc-demo--local-agent", runtime.Config{}); err != nil {
		t.Fatalf("start local: %v", err)
	}

	names, err := h.ListRunningFresh("gc-demo-")
	if err != nil {
		t.Fatalf("ListRunningFresh: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("ListRunningFresh = %v, want local and late remote sessions", names)
	}
	if remote.freshCalls != 1 {
		t.Fatalf("remote ListRunningFresh calls = %d, want 1", remote.freshCalls)
	}
}

func TestListRunning_PartialFailure(t *testing.T) {
	local := runtime.NewFake()
	remote := runtime.NewFailFake()
	h := New(local, remote, isRemote)

	_ = local.Start(context.Background(), "gc-demo--local-agent", runtime.Config{})

	names, err := h.ListRunning("gc-demo-")
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning error = %v, want partial list error", err)
	}
	if len(names) != 1 {
		t.Fatalf("expected 1 session from healthy backend, got %d", len(names))
	}
}

func TestListRunning_BothFail(t *testing.T) {
	local := runtime.NewFailFake()
	remote := runtime.NewFailFake()
	h := New(local, remote, isRemote)

	_, err := h.ListRunning("gc-demo-")
	if err == nil {
		t.Fatal("expected error when both backends fail")
	}
}

func TestAttach_RoutesCorrectly(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	_ = h.Start(context.Background(), "local-agent", runtime.Config{})
	_ = h.Start(context.Background(), "remote-agent-1", runtime.Config{})

	if err := h.Attach("local-agent"); err != nil {
		t.Errorf("attach local: %v", err)
	}
	if err := h.Attach("remote-agent-1"); err != nil {
		t.Errorf("attach remote: %v", err)
	}

	// Verify calls went to correct backends.
	var localAttach, remoteAttach int
	for _, c := range local.Calls {
		if c.Method == "Attach" {
			localAttach++
		}
	}
	for _, c := range remote.Calls {
		if c.Method == "Attach" {
			remoteAttach++
		}
	}
	if localAttach != 1 {
		t.Errorf("expected 1 local attach, got %d", localAttach)
	}
	if remoteAttach != 1 {
		t.Errorf("expected 1 remote attach, got %d", remoteAttach)
	}
}

func TestStop_RoutesCorrectly(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	_ = h.Start(context.Background(), "local-agent", runtime.Config{})
	_ = h.Start(context.Background(), "remote-agent-1", runtime.Config{})

	if err := h.Stop("local-agent"); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop("remote-agent-1"); err != nil {
		t.Fatal(err)
	}

	if local.IsRunning("local-agent") {
		t.Error("local-agent should be stopped")
	}
	if remote.IsRunning("remote-agent-1") {
		t.Error("remote-agent-1 should be stopped")
	}
}

func TestPendingAndRespond_RouteToBackend(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	_ = h.Start(context.Background(), "remote-agent-1", runtime.Config{})
	remote.SetPendingInteraction("remote-agent-1", &runtime.PendingInteraction{RequestID: "req-1"})

	pending, err := h.Pending("remote-agent-1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil || pending.RequestID != "req-1" {
		t.Fatalf("Pending = %#v, want req-1", pending)
	}
	if err := h.Respond("remote-agent-1", runtime.InteractionResponse{RequestID: "req-1", Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got := remote.Responses["remote-agent-1"]; len(got) != 1 || got[0].Action != "approve" {
		t.Fatalf("Responses = %#v, want single approve", got)
	}
}

func TestPendingUnsupportedWhenBackendLacksInteractionSupport(t *testing.T) {
	local := &runtimeNoInteractionProvider{Provider: runtime.NewFake()}
	remote := runtime.NewFake()
	h := New(local, remote, isRemote)

	_, err := h.Pending("local-agent")
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("Pending error = %v, want ErrInteractionUnsupported", err)
	}
}

type runtimeNoInteractionProvider struct {
	runtime.Provider
}

type deadRuntimeCheckProvider struct {
	*runtime.Fake
	dead   map[string]bool
	errs   map[string]error
	checks []string
}

func newDeadRuntimeCheckProvider() *deadRuntimeCheckProvider {
	return &deadRuntimeCheckProvider{
		Fake: runtime.NewFake(),
		dead: make(map[string]bool),
		errs: make(map[string]error),
	}
}

func (p *deadRuntimeCheckProvider) IsDeadRuntimeSession(name string) (bool, error) {
	p.checks = append(p.checks, name)
	if err := p.errs[name]; err != nil {
		return false, err
	}
	return p.dead[name], nil
}

func TestIsDeadRuntimeSessionDelegatesToRoutedChecker(t *testing.T) {
	local := newDeadRuntimeCheckProvider()
	remote := newDeadRuntimeCheckProvider()
	remote.dead["remote-agent-1"] = true
	h := New(local, remote, isRemote)

	dead, err := h.IsDeadRuntimeSession("remote-agent-1")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if !dead {
		t.Fatal("IsDeadRuntimeSession = false, want true from routed remote checker")
	}
	if len(local.checks) != 0 {
		t.Fatalf("local checks = %v, want none", local.checks)
	}
	if got := remote.checks; len(got) != 1 || got[0] != "remote-agent-1" {
		t.Fatalf("remote checks = %v, want [remote-agent-1]", got)
	}
}

func TestIsDeadRuntimeSessionReturnsFalseWhenRoutedBackendLacksChecker(t *testing.T) {
	local := runtime.NewFake()
	remote := newDeadRuntimeCheckProvider()
	remote.dead["local-agent"] = true
	h := New(local, remote, isRemote)

	dead, err := h.IsDeadRuntimeSession("local-agent")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false for non-checker routed backend")
	}
	if len(remote.checks) != 0 {
		t.Fatalf("remote checks = %v, want none for local-routed session", remote.checks)
	}
}

func TestIsDeadRuntimeSessionReturnsRoutedCheckerError(t *testing.T) {
	local := newDeadRuntimeCheckProvider()
	remote := newDeadRuntimeCheckProvider()
	remote.errs["remote-agent-1"] = fmt.Errorf("runtime unavailable")
	h := New(local, remote, isRemote)

	dead, err := h.IsDeadRuntimeSession("remote-agent-1")
	if err == nil {
		t.Fatal("IsDeadRuntimeSession error = nil, want routed checker error")
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false on checker error")
	}
	if !strings.Contains(err.Error(), "runtime unavailable") {
		t.Fatalf("IsDeadRuntimeSession error = %v, want runtime unavailable", err)
	}
}
