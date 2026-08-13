package k8s

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestProviderImplementsInterface(_ *testing.T) {
	// Compile-time check is in provider.go, but verify at test time too.
	var _ runtime.Provider = (*Provider)(nil)
}

func TestManagedServiceAliasDefaults(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "canonical-dolt.example.com")
	t.Setenv("GC_DOLT_PORT", "4407")

	host, port, err := managedServiceAlias()
	if err != nil {
		t.Fatalf("managedServiceAlias() error = %v", err)
	}
	if host != podManagedDoltHost {
		t.Fatalf("host = %q, want %q", host, podManagedDoltHost)
	}
	if port != podManagedDoltPort {
		t.Fatalf("port = %q, want %q", port, podManagedDoltPort)
	}
}

func TestManagedServiceAliasCompatOverride(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "canonical-dolt.example.com")
	t.Setenv("GC_DOLT_PORT", "4407")
	t.Setenv("GC_K8S_DOLT_HOST", "legacy-dolt.example.com")
	t.Setenv("GC_K8S_DOLT_PORT", "3308")

	host, port, err := managedServiceAlias()
	if err != nil {
		t.Fatalf("managedServiceAlias() error = %v", err)
	}
	if host != "legacy-dolt.example.com" {
		t.Fatalf("host = %q, want legacy-dolt.example.com", host)
	}
	if port != "3308" {
		t.Fatalf("port = %q, want 3308", port)
	}
}

func TestManagedServiceAliasRejectsPartialCompatOverride(t *testing.T) {
	t.Setenv("GC_K8S_DOLT_HOST", "legacy-dolt.example.com")

	_, _, err := managedServiceAlias()
	if err == nil {
		t.Fatal("expected partial compatibility override to fail")
	}
	if got := err.Error(); got != "requires both GC_K8S_DOLT_HOST and GC_K8S_DOLT_PORT when either is set" {
		t.Fatalf("managedServiceAlias() error = %q", got)
	}
}

func TestParseSchedulingEnvHappyPath(t *testing.T) {
	clearSchedulingEnv(t)
	t.Setenv("GC_K8S_NODE_SELECTOR", `{"workload":"gc-agents"}`)
	t.Setenv("GC_K8S_TOLERATIONS", `[{"key":"gc-agents","operator":"Exists","effect":"NoSchedule","tolerationSeconds":60}]`)
	t.Setenv("GC_K8S_AFFINITY", `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"node-type","operator":"In","values":["gpu"]}]}]}}}`)
	t.Setenv("GC_K8S_PRIORITY_CLASS_NAME", "gc-agent-high")

	scheduling, err := parseSchedulingEnv()
	if err != nil {
		t.Fatalf("parseSchedulingEnv: %v", err)
	}
	if scheduling.nodeSelector["workload"] != "gc-agents" {
		t.Fatalf("nodeSelector[workload] = %q, want gc-agents", scheduling.nodeSelector["workload"])
	}
	if len(scheduling.tolerations) != 1 {
		t.Fatalf("len(tolerations) = %d, want 1", len(scheduling.tolerations))
	}
	if scheduling.tolerations[0].TolerationSeconds == nil || *scheduling.tolerations[0].TolerationSeconds != 60 {
		t.Fatalf("tolerationSeconds = %v, want 60", scheduling.tolerations[0].TolerationSeconds)
	}
	if scheduling.affinity == nil ||
		scheduling.affinity.NodeAffinity == nil ||
		scheduling.affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("affinity did not parse required node affinity: %#v", scheduling.affinity)
	}
	if got := scheduling.priorityClassName; got != "gc-agent-high" {
		t.Fatalf("priorityClassName = %q, want gc-agent-high", got)
	}
}

func TestParseSchedulingEnvRejectsMalformedJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "node selector", key: "GC_K8S_NODE_SELECTOR"},
		{name: "tolerations", key: "GC_K8S_TOLERATIONS"},
		{name: "affinity", key: "GC_K8S_AFFINITY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSchedulingEnv(t)
			t.Setenv(tc.key, "{")

			_, err := parseSchedulingEnv()
			if err == nil {
				t.Fatal("expected malformed JSON to fail")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error = %q, want to mention %s", err, tc.key)
			}
		})
	}
}

func TestParseSchedulingEnvEmptyAndNullAffinitySemantics(t *testing.T) {
	t.Run("empty strings are unset", func(t *testing.T) {
		clearSchedulingEnv(t)

		scheduling, err := parseSchedulingEnv()
		if err != nil {
			t.Fatalf("parseSchedulingEnv: %v", err)
		}
		if scheduling.nodeSelector != nil {
			t.Fatalf("nodeSelector = %#v, want nil", scheduling.nodeSelector)
		}
		if len(scheduling.tolerations) != 0 {
			t.Fatalf("len(tolerations) = %d, want 0", len(scheduling.tolerations))
		}
		if scheduling.affinity != nil {
			t.Fatalf("affinity = %#v, want nil", scheduling.affinity)
		}
		if scheduling.priorityClassName != "" {
			t.Fatalf("priorityClassName = %q, want empty", scheduling.priorityClassName)
		}
	})

	t.Run("affinity null is unset", func(t *testing.T) {
		clearSchedulingEnv(t)
		t.Setenv("GC_K8S_AFFINITY", "null")

		scheduling, err := parseSchedulingEnv()
		if err != nil {
			t.Fatalf("parseSchedulingEnv: %v", err)
		}
		if scheduling.affinity != nil {
			t.Fatalf("affinity = %#v, want nil", scheduling.affinity)
		}
	})

	t.Run("affinity empty object is explicit empty", func(t *testing.T) {
		clearSchedulingEnv(t)
		t.Setenv("GC_K8S_AFFINITY", "{}")

		scheduling, err := parseSchedulingEnv()
		if err != nil {
			t.Fatalf("parseSchedulingEnv: %v", err)
		}
		if scheduling.affinity == nil {
			t.Fatal("affinity = nil, want explicit empty affinity")
		}
		if scheduling.affinity.NodeAffinity != nil {
			t.Fatalf("NodeAffinity = %#v, want nil", scheduling.affinity.NodeAffinity)
		}
	})
}

func clearSchedulingEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GC_K8S_NODE_SELECTOR",
		"GC_K8S_TOLERATIONS",
		"GC_K8S_AFFINITY",
		"GC_K8S_PRIORITY_CLASS_NAME",
	} {
		t.Setenv(key, "")
	}
}

func TestProjectedPodStoreRootPrefersGCStoreRoot(t *testing.T) {
	cfg := runtime.Config{
		WorkDir: "/host/city/workspaces/agent",
		Env: map[string]string{
			"GC_CITY":       "/host/city",
			"GC_STORE_ROOT": "/host/city/rigs/frontend",
		},
	}

	podWorkDir := projectedPodWorkDir(cfg)
	if podWorkDir != "/workspace/workspaces/agent" {
		t.Fatalf("projectedPodWorkDir = %q, want %q", podWorkDir, "/workspace/workspaces/agent")
	}
	if got := projectedPodStoreRoot(cfg, podWorkDir); got != "/workspace/rigs/frontend" {
		t.Fatalf("projectedPodStoreRoot = %q, want %q", got, "/workspace/rigs/frontend")
	}
}

func TestProjectedPodExternalRigSeparatesCityAndLedgerPaths(t *testing.T) {
	cfg := runtime.Config{
		WorkDir: "/data/rigs/infra-blocks",
		Command: "cd /data/rigs/infra-blocks && pi",
		Env: map[string]string{
			"GC_CITY":       "/data/city",
			"GC_DIR":        "/data/rigs/infra-blocks",
			"GC_RIG_ROOT":   "/data/rigs/infra-blocks",
			"GC_STORE_ROOT": "/data/rigs/infra-blocks",
			"BEADS_DIR":     "/data/rigs/infra-blocks/.beads",
			"GT_ROOT":       "/data/rigs/infra-blocks",
			"GC_PACK_DIR":   "/data/rigs/infra-blocks/.gc/packs",
		},
		PreStart: []string{"test -f /data/rigs/infra-blocks/.beads/metadata.json"},
	}

	podWorkDir := projectedPodWorkDir(cfg)
	if podWorkDir != "/workspace/rig" {
		t.Fatalf("projectedPodWorkDir = %q, want /workspace/rig", podWorkDir)
	}
	if got := projectedPodStoreRoot(cfg, podWorkDir); got != "/workspace/rig" {
		t.Fatalf("projectedPodStoreRoot = %q, want /workspace/rig", got)
	}

	commandBytes, err := base64.StdEncoding.DecodeString(agentCommandB64(cfg))
	if err != nil {
		t.Fatalf("decode agent command: %v", err)
	}
	if got := string(commandBytes); got != "cd /workspace/rig && pi" {
		t.Fatalf("projected agent command = %q, want external rig path remapped", got)
	}

	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("gc-external-rig", cfg, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	agent := pod.Spec.Containers[0]
	if agent.WorkingDir != "/workspace/rig" {
		t.Fatalf("agent.WorkingDir = %q, want /workspace/rig", agent.WorkingDir)
	}
	env := make(map[string]string, len(agent.Env))
	for _, item := range agent.Env {
		env[item.Name] = item.Value
	}
	for key, want := range map[string]string{
		"GC_CITY":       "/workspace",
		"GC_DIR":        "/workspace/rig",
		"GC_RIG_ROOT":   "/workspace/rig",
		"GC_STORE_ROOT": "/workspace/rig",
		"BEADS_DIR":     "/workspace/rig/.beads",
		"GT_ROOT":       "/workspace/rig",
		"GC_PACK_DIR":   "/workspace/rig/.gc/packs",
	} {
		if got := env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	wantPreStart := base64.StdEncoding.EncodeToString(
		[]byte("test -f /workspace/rig/.beads/metadata.json"),
	)
	if len(agent.Args) != 1 || !strings.Contains(agent.Args[0], wantPreStart) {
		t.Fatalf("agent pre-start command does not contain remapped external rig path")
	}
}

func TestIsRunning(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// No pod → not running.
	if p.IsRunning("gc-test-agent") {
		t.Error("IsRunning returned true for non-existent session")
	}

	// Pod exists + tmux alive → running.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent", []string{"tmux", "has-session", "-t", "main"}, "", nil)

	if !p.IsRunning("gc-test-agent") {
		t.Error("IsRunning returned false for running session")
	}

	// Pod exists but tmux dead → not running.
	fake.setExecResult("gc-test-agent", []string{"tmux", "has-session", "-t", "main"}, "",
		fmt.Errorf("no session: main"))

	if p.IsRunning("gc-test-agent") {
		t.Error("IsRunning returned true for session with dead tmux")
	}
}

// TestIsRunningCheckedDistinguishesConfirmedFromInconclusive proves
// IsRunningChecked's whole reason to exist: a probe that fails to complete
// (a lookup or exec transport failure) must report a non-nil error rather
// than collapsing to the same false a confirmed negative returns, so a
// destructive caller can tell "definitely not running" apart from "could
// not tell." IsRunning (the plain bool method) must keep collapsing every
// case to false — that contract does not change for existing callers.
func TestIsRunningCheckedDistinguishesConfirmedFromInconclusive(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Confirmed negative: no pod at all.
	running, err := p.IsRunningChecked("gc-test-agent")
	if running {
		t.Error("IsRunningChecked reported running with no pod")
	}
	if err != nil {
		t.Errorf("IsRunningChecked returned an error for a confirmed-absent pod: %v", err)
	}

	// Inconclusive: the pod lookup itself fails (e.g. a timed-out LIST).
	fake.listErr = context.DeadlineExceeded
	running, err = p.IsRunningChecked("gc-test-agent")
	if running {
		t.Error("IsRunningChecked reported running despite a failed lookup")
	}
	if err == nil {
		t.Fatal("IsRunningChecked swallowed a lookup failure instead of reporting it as inconclusive")
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("IsRunningChecked error = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
	}
	if p.IsRunning("gc-test-agent") {
		t.Error("IsRunning true despite a failed lookup")
	}
	fake.listErr = nil

	// Confirmed negative: pod exists, tmux ran and confirmed no session
	// (an exit code, not a transport failure).
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent", []string{"tmux", "has-session", "-t", tmuxSession}, "",
		execerr.CodeExitError{Err: fmt.Errorf("no session: main"), Code: 1})
	running, err = p.IsRunningChecked("gc-test-agent")
	if running {
		t.Error("IsRunningChecked reported running for a confirmed-dead tmux session")
	}
	if err != nil {
		t.Errorf("IsRunningChecked returned an error for a confirmed exit-code result: %v", err)
	}

	// Inconclusive: exec itself fails to complete (transport), not a
	// command exit code.
	fake.setExecResult("gc-test-agent", []string{"tmux", "has-session", "-t", tmuxSession}, "",
		fmt.Errorf("stream error: broken pipe"))
	running, err = p.IsRunningChecked("gc-test-agent")
	if running {
		t.Error("IsRunningChecked reported running despite an exec transport failure")
	}
	if err == nil {
		t.Fatal("IsRunningChecked swallowed an exec transport failure instead of reporting it as inconclusive")
	}
	if p.IsRunning("gc-test-agent") {
		t.Error("IsRunning true despite an exec transport failure")
	}
}

// TestIsRunningCheckedSnapshotPathDistinguishesConfirmedFromInconclusive
// re-proves the confirmed-vs-inconclusive distinction with
// runningPodCacheTTL > 0 (the namespace-snapshot path production uses by
// default — see NewProvider). findRunningPodFromSnapshot needed its own
// runtime.ErrSessionNotFound wrap, matching the non-snapshot findRunningPod,
// for IsRunningChecked to make this distinction under the default config;
// without it every confirmed-absent session would misreport as inconclusive.
func TestIsRunningCheckedSnapshotPathDistinguishesConfirmedFromInconclusive(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.runningPodCacheTTL = time.Hour

	// Confirmed negative: snapshot refreshes cleanly, session absent from it.
	running, err := p.IsRunningChecked("gc-test-agent")
	if running {
		t.Error("IsRunningChecked reported running with no pod in the snapshot")
	}
	if err != nil {
		t.Errorf("IsRunningChecked returned an error for a confirmed-absent snapshot entry: %v", err)
	}

	// Inconclusive: the snapshot refresh itself fails.
	p.invalidateRunningPodSnapshot()
	fake.listErr = context.DeadlineExceeded
	running, err = p.IsRunningChecked("gc-test-agent")
	if running {
		t.Error("IsRunningChecked reported running despite a failed snapshot refresh")
	}
	if err == nil {
		t.Fatal("IsRunningChecked swallowed a snapshot refresh failure instead of reporting it as inconclusive")
	}
}

func TestIsAttached(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// No pod → not attached.
	if p.IsAttached("gc-test-agent") {
		t.Error("IsAttached returned true for non-existent session")
	}

	// Pod exists, tmux reports attached.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_attached}"}, "1\n", nil)
	if !p.IsAttached("gc-test-agent") {
		t.Error("IsAttached returned false for an attached session")
	}

	// Pod exists, tmux reports not attached.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_attached}"}, "0\n", nil)
	if p.IsAttached("gc-test-agent") {
		t.Error("IsAttached returned true for a detached session")
	}
}

// TestIsAttachedCheckedDistinguishesConfirmedFromInconclusive is the
// IsAttached analogue of TestIsRunningCheckedDistinguishesConfirmedFromInconclusive.
func TestIsAttachedCheckedDistinguishesConfirmedFromInconclusive(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Confirmed negative: no pod at all.
	attached, err := p.IsAttachedChecked("gc-test-agent")
	if attached {
		t.Error("IsAttachedChecked reported attached with no pod")
	}
	if err != nil {
		t.Errorf("IsAttachedChecked returned an error for a confirmed-absent pod: %v", err)
	}

	// Inconclusive: the pod lookup itself fails (e.g. a timed-out LIST).
	fake.listErr = context.DeadlineExceeded
	attached, err = p.IsAttachedChecked("gc-test-agent")
	if attached {
		t.Error("IsAttachedChecked reported attached despite a failed lookup")
	}
	if err == nil {
		t.Fatal("IsAttachedChecked swallowed a lookup failure instead of reporting it as inconclusive")
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("IsAttachedChecked error = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
	}
	if p.IsAttached("gc-test-agent") {
		t.Error("IsAttached true despite a failed lookup")
	}
	fake.listErr = nil

	// Confirmed negative: pod exists, exec ran and returned a confirmed
	// exit code (e.g. a session that disappeared between lookup and exec),
	// not a transport failure.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_attached}"}, "",
		execerr.CodeExitError{Err: fmt.Errorf("no session: main"), Code: 1})
	attached, err = p.IsAttachedChecked("gc-test-agent")
	if attached {
		t.Error("IsAttachedChecked reported attached for a confirmed exit-code result")
	}
	if err != nil {
		t.Errorf("IsAttachedChecked returned an error for a confirmed exit-code result: %v", err)
	}

	// Inconclusive: exec itself fails to complete (transport).
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_attached}"}, "",
		fmt.Errorf("stream error: broken pipe"))
	attached, err = p.IsAttachedChecked("gc-test-agent")
	if attached {
		t.Error("IsAttachedChecked reported attached despite an exec transport failure")
	}
	if err == nil {
		t.Fatal("IsAttachedChecked swallowed an exec transport failure instead of reporting it as inconclusive")
	}
	if p.IsAttached("gc-test-agent") {
		t.Error("IsAttached true despite an exec transport failure")
	}
}

func TestRunningPodLookupSharesOneNamespaceSnapshotAcrossSessions(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.runningPodCacheTTL = time.Hour

	addRunningPod(fake, "agent-a", "session-a")
	addRunningPod(fake, "agent-b", "session-b")

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"session-a", "session-b"} {
		go func() {
			<-start
			_, err := p.findRunningPod(context.Background(), name)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("findRunningPod() error = %v", err)
		}
	}
	if p.IsRunning("missing") {
		t.Fatal("missing session reported running")
	}

	var listCalls int
	for _, call := range fake.calls {
		if call.method != "listPods" {
			continue
		}
		listCalls++
		if call.selector != "" {
			t.Fatalf("listPods selector = %q, want one namespace-wide snapshot", call.selector)
		}
	}
	if listCalls != 1 {
		t.Fatalf("listPods calls = %d, want 1 shared snapshot", listCalls)
	}
}

type snapshotListOps struct {
	*fakeK8sOps
	pods            []corev1.Pod
	listErr         error
	requireDeadline bool
	entered         chan struct{}
	release         chan struct{}
	enteredOnce     sync.Once
	listCalls       atomic.Int32
}

type staleSnapshotRaceOps struct {
	*fakeK8sOps
	mu        sync.Mutex
	pods      []corev1.Pod
	entered   chan struct{}
	release   chan struct{}
	listCalls atomic.Int32
}

type mutateThenErrorOps struct {
	*fakeK8sOps
	createEntered chan struct{}
	createRelease chan struct{}
	deleteEntered chan struct{}
	deleteRelease chan struct{}
	deleteError   bool
}

type cleanupDeleteRaceOps struct {
	*fakeK8sOps
	deleteEntered chan struct{}
	deleteRelease chan struct{}
}

func (o *cleanupDeleteRaceOps) deletePod(ctx context.Context, name string, uid types.UID, grace int64) error {
	close(o.deleteEntered)
	<-o.deleteRelease
	if err := o.fakeK8sOps.deletePod(ctx, name, uid, grace); err != nil {
		return err
	}
	return errors.New("cleanup delete response lost after mutation")
}

func (o *mutateThenErrorOps) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	close(o.createEntered)
	<-o.createRelease
	created, err := o.fakeK8sOps.createPod(ctx, pod)
	if err != nil {
		return created, err
	}
	return created, errors.New("create response lost after mutation")
}

func (o *mutateThenErrorOps) deletePod(ctx context.Context, name string, uid types.UID, grace int64) error {
	close(o.deleteEntered)
	<-o.deleteRelease
	if err := o.fakeK8sOps.deletePod(ctx, name, uid, grace); err != nil {
		return err
	}
	if o.deleteError {
		return errors.New("delete response lost after mutation")
	}
	return nil
}

func (o *staleSnapshotRaceOps) setPods(pods ...corev1.Pod) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pods = append([]corev1.Pod(nil), pods...)
}

func (o *staleSnapshotRaceOps) listPods(ctx context.Context, _, _ string) ([]corev1.Pod, error) {
	o.mu.Lock()
	pods := make([]corev1.Pod, len(o.pods))
	for i := range o.pods {
		pods[i] = *o.pods[i].DeepCopy()
	}
	o.mu.Unlock()
	if o.listCalls.Add(1) == 1 {
		close(o.entered)
		select {
		case <-o.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return pods, nil
}

func newSnapshotListOps(pods ...corev1.Pod) *snapshotListOps {
	return &snapshotListOps{fakeK8sOps: newFakeK8sOps(), pods: pods}
}

func awaitSnapshotTestValue[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
		t.Fatal("timed out waiting for running-pod snapshot test signal")
		var zero T
		return zero
	}
}

func (o *snapshotListOps) listPods(ctx context.Context, _, _ string) ([]corev1.Pod, error) {
	o.listCalls.Add(1)
	if o.entered != nil {
		o.enteredOnce.Do(func() { close(o.entered) })
	}
	if o.release != nil {
		select {
		case <-o.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if o.requireDeadline {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("running-pod snapshot has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > 10*time.Second {
			return nil, fmt.Errorf("running-pod snapshot deadline is not tightly bounded: %s", remaining)
		}
	}
	if o.listErr != nil {
		return nil, o.listErr
	}
	pods := make([]corev1.Pod, len(o.pods))
	for i := range o.pods {
		pods[i] = *o.pods[i].DeepCopy()
	}
	return pods, nil
}

func runningPodForSnapshot(podName, sessionName string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Labels:      map[string]string{"app": "gc-agent", "gc-session": SanitizeLabel(sessionName)},
			Annotations: map[string]string{"gc-session-name": sessionName},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestListRunningSharesSnapshotAcrossFactoryBurst(t *testing.T) {
	ops := newSnapshotListOps(
		runningPodForSnapshot("pod-a", "gc-city-a-worker-1"),
		runningPodForSnapshot("pod-b", "gc-city-b-worker-1"),
	)
	p := newProviderWithOps(ops)
	p.runningPodCacheTTL = time.Hour

	for i := range 117 {
		prefix := "gc-city-a-"
		if i%2 == 1 {
			prefix = "gc-city-b-"
		}
		names, err := p.ListRunning(prefix)
		if err != nil {
			t.Fatalf("ListRunning(%q): %v", prefix, err)
		}
		if len(names) != 1 {
			t.Fatalf("ListRunning(%q) = %v, want one session", prefix, names)
		}
	}

	if got := ops.listCalls.Load(); got != 1 {
		t.Fatalf("Kubernetes LIST calls for 117 ListRunning calls = %d, want 1", got)
	}
}

func TestListRunningAndSessionLookupShareSnapshot(t *testing.T) {
	legacyPod := runningPodForSnapshot("pod-b", "gc-city-legacy-1")
	legacyPod.Labels["app"] = "legacy-session"
	ops := newSnapshotListOps(
		runningPodForSnapshot("pod-a", "gc-city-worker-1"),
		legacyPod,
	)
	p := newProviderWithOps(ops)
	p.runningPodCacheTTL = time.Hour

	names, err := p.ListRunning("gc-city-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 1 || names[0] != "gc-city-worker-1" {
		t.Fatalf("ListRunning = %v, want [gc-city-worker-1]", names)
	}
	podName, err := p.findRunningPod(context.Background(), "gc-city-worker-1")
	if err != nil {
		t.Fatalf("findRunningPod: %v", err)
	}
	if podName != "pod-a" {
		t.Fatalf("findRunningPod = %q, want pod-a", podName)
	}
	legacyPodName, err := p.findRunningPod(context.Background(), "gc-city-legacy-1")
	if err != nil {
		t.Fatalf("findRunningPod legacy session: %v", err)
	}
	if legacyPodName != "pod-b" {
		t.Fatalf("findRunningPod legacy session = %q, want pod-b", legacyPodName)
	}
	if got := ops.listCalls.Load(); got != 1 {
		t.Fatalf("Kubernetes LIST calls across list and lookup = %d, want 1", got)
	}
}

func TestListRunningFreshBypassesObservationSnapshotThroughProductionWrapper(t *testing.T) {
	ops := newSnapshotListOps()
	raw := newProviderWithOps(ops)
	raw.runningPodCacheTTL = time.Hour
	provider := newSeamBacked(raw)

	names, err := provider.ListRunning("")
	if err != nil {
		t.Fatalf("priming ListRunning: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("priming ListRunning = %v, want no sessions", names)
	}
	ops.pods = []corev1.Pod{runningPodForSnapshot("pod-late", "gc-city-worker-late")}

	names, err = provider.ListRunning("")
	if err != nil {
		t.Fatalf("cached ListRunning: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("cached ListRunning = %v, want stale empty observation snapshot", names)
	}
	fresh, ok := provider.(runtime.FreshRunningSessionLister)
	if !ok {
		t.Fatalf("production k8s provider %T does not preserve fresh running-session inventory", provider)
	}
	names, err = fresh.ListRunningFresh("")
	if err != nil {
		t.Fatalf("ListRunningFresh: %v", err)
	}
	if len(names) != 1 || names[0] != "gc-city-worker-late" {
		t.Fatalf("ListRunningFresh = %v, want [gc-city-worker-late]", names)
	}
	if got := ops.listCalls.Load(); got != 2 {
		t.Fatalf("Kubernetes LIST calls across cached observations and fresh inventory = %d, want 2", got)
	}
}

func TestRunningPodSnapshotCoalescesConcurrentFailure(t *testing.T) {
	wantErr := errors.New("Kubernetes API unavailable")
	ops := newSnapshotListOps()
	ops.listErr = wantErr
	ops.entered = make(chan struct{})
	ops.release = make(chan struct{})
	p := newProviderWithOps(ops)
	p.runningPodCacheTTL = time.Hour

	oldProcs := goruntime.GOMAXPROCS(1)
	t.Cleanup(func() { goruntime.GOMAXPROCS(oldProcs) })

	const callers = 32
	start := make(chan struct{})
	ready := make(chan struct{}, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			ready <- struct{}{}
			_, err := p.ListRunning("")
			errs <- err
		}()
	}
	close(start)
	for range callers {
		awaitSnapshotTestValue(t, ready)
	}
	awaitSnapshotTestValue(t, ops.entered)
	// With one P, every caller reaches either the fake LIST or the shared
	// singleflight wait before this goroutine resumes after yielding.
	goruntime.Gosched()
	close(ops.release)

	for range callers {
		if err := awaitSnapshotTestValue(t, errs); !errors.Is(err, wantErr) {
			t.Fatalf("ListRunning error = %v, want %v", err, wantErr)
		}
	}
	if got := ops.listCalls.Load(); got != 1 {
		t.Fatalf("Kubernetes LIST calls for %d concurrent failures = %d, want 1", callers, got)
	}
}

func TestRunningPodSnapshotUsesBoundedContext(t *testing.T) {
	ops := newSnapshotListOps(runningPodForSnapshot("pod-a", "gc-city-worker-1"))
	ops.requireDeadline = true
	p := newProviderWithOps(ops)
	p.runningPodCacheTTL = time.Hour

	if _, err := p.ListRunning(""); err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if got := ops.listCalls.Load(); got != 1 {
		t.Fatalf("Kubernetes LIST calls = %d, want 1", got)
	}
}

func TestRunningPodSnapshotExpiresAndDoesNotCacheRefreshFailure(t *testing.T) {
	ops := newSnapshotListOps(runningPodForSnapshot("pod-a", "gc-city-worker-1"))
	p := newProviderWithOps(ops)
	p.runningPodCacheTTL = time.Hour

	if _, err := p.ListRunning(""); err != nil {
		t.Fatalf("priming ListRunning: %v", err)
	}
	p.runningPodCacheMu.Lock()
	p.runningPodCacheAt = time.Now().Add(-2 * time.Hour)
	p.runningPodCacheMu.Unlock()

	wantErr := errors.New("Kubernetes API unavailable")
	ops.listErr = wantErr
	if _, err := p.ListRunning(""); !errors.Is(err, wantErr) {
		t.Fatalf("expired ListRunning error = %v, want %v", err, wantErr)
	}

	ops.listErr = nil
	ops.pods = []corev1.Pod{runningPodForSnapshot("pod-b", "gc-city-worker-2")}
	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("retrying ListRunning: %v", err)
	}
	if len(names) != 1 || names[0] != "gc-city-worker-2" {
		t.Fatalf("retrying ListRunning = %v, want [gc-city-worker-2]", names)
	}
	if got := ops.listCalls.Load(); got != 3 {
		t.Fatalf("Kubernetes LIST calls across success, failure, retry = %d, want 3", got)
	}
}

func TestRunningPodSnapshotCallerCanCancelSharedWait(t *testing.T) {
	ops := newSnapshotListOps(runningPodForSnapshot("pod-a", "gc-city-worker-1"))
	ops.entered = make(chan struct{})
	ops.release = make(chan struct{})
	p := newProviderWithOps(ops)
	p.runningPodCacheTTL = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.runningPodSnapshot(ctx)
		result <- err
	}()
	awaitSnapshotTestValue(t, ops.entered)
	cancel()
	if err := awaitSnapshotTestValue(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("runningPodSnapshot error = %v, want context.Canceled", err)
	}
	close(ops.release)
}

func TestRunningPodSnapshotInvalidationRejectsPreMutationRefresh(t *testing.T) {
	ops := &staleSnapshotRaceOps{
		fakeK8sOps: newFakeK8sOps(),
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ops.setPods(runningPodForSnapshot("pod-old", "gc-city-old"))
	p := newProviderWithOps(ops)
	p.runningPodCacheTTL = time.Hour

	first := make(chan []string, 1)
	go func() {
		names, _ := p.ListRunning("")
		first <- names
	}()
	awaitSnapshotTestValue(t, ops.entered)
	p.invalidateRunningPodSnapshot()
	ops.setPods(runningPodForSnapshot("pod-new", "gc-city-new"))
	close(ops.release)
	if names := awaitSnapshotTestValue(t, first); len(names) != 1 || names[0] != "gc-city-old" {
		t.Fatalf("pre-mutation caller = %v, want its original [gc-city-old] snapshot", names)
	}

	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("post-mutation ListRunning: %v", err)
	}
	if len(names) != 1 || names[0] != "gc-city-new" {
		t.Fatalf("post-mutation ListRunning = %v, want [gc-city-new]", names)
	}
	if got := ops.listCalls.Load(); got != 2 {
		t.Fatalf("Kubernetes LIST calls across invalidation race = %d, want 2", got)
	}
}

func TestStartInvalidatesSnapshotBeforeCreateThatMutatesThenErrors(t *testing.T) {
	ops := &mutateThenErrorOps{
		fakeK8sOps:    newFakeK8sOps(),
		createEntered: make(chan struct{}),
		createRelease: make(chan struct{}),
		deleteEntered: make(chan struct{}),
		deleteRelease: make(chan struct{}),
	}
	p := newProviderWithOps(ops)
	p.prebaked = true
	p.runningPodCacheTTL = time.Hour
	if names, err := p.ListRunning(""); err != nil || len(names) != 0 {
		t.Fatalf("prime ListRunning = %v, %v; want empty", names, err)
	}

	startErr := make(chan error, 1)
	go func() {
		startErr <- p.Start(context.Background(), "worker", runtime.Config{Command: "agent", Env: map[string]string{"GC_INSTANCE_TOKEN": "tok-worker"}})
	}()
	awaitSnapshotTestValue(t, ops.createEntered)
	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning during mutate-then-error create: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("ListRunning before mutate-then-error create commits = %v, want empty stale snapshot", names)
	}
	close(ops.createRelease)
	if err := awaitSnapshotTestValue(t, startErr); err == nil {
		t.Fatal("Start returned nil after injected mutate-then-error create")
	}
	names, err = p.ListRunning("")
	if err != nil || len(names) != 1 || names[0] != "worker" {
		t.Fatalf("ListRunning after mutate-then-error create = %v, %v; want [worker]", names, err)
	}
}

func TestStopInvalidatesSnapshotBeforeDeleteThatMutatesThenErrors(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "worker", "worker")
	ops := &mutateThenErrorOps{
		fakeK8sOps:    fake,
		createEntered: make(chan struct{}),
		createRelease: make(chan struct{}),
		deleteEntered: make(chan struct{}),
		deleteRelease: make(chan struct{}),
		deleteError:   true,
	}
	p := newProviderWithOps(ops)
	p.runningPodCacheTTL = time.Hour
	if names, err := p.ListRunning(""); err != nil || len(names) != 1 {
		t.Fatalf("prime ListRunning = %v, %v; want [worker]", names, err)
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- p.Stop("worker") }()
	awaitSnapshotTestValue(t, ops.deleteEntered)
	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning during mutate-then-error delete: %v", err)
	}
	if len(names) != 1 || names[0] != "worker" {
		t.Fatalf("ListRunning before mutate-then-error delete commits = %v, want stale [worker]", names)
	}
	close(ops.deleteRelease)
	if err := awaitSnapshotTestValue(t, stopErr); err == nil {
		t.Fatal("Stop returned nil after injected mutate-then-error delete")
	}
	names, err = p.ListRunning("")
	if err != nil || len(names) != 0 {
		t.Fatalf("ListRunning after mutate-then-error delete = %v, %v; want empty", names, err)
	}
}

func TestStartCleanupInvalidatesSnapshotAfterDeleteThatMutatesThenErrors(t *testing.T) {
	fake := newFakeK8sOps()
	ops := &cleanupDeleteRaceOps{
		fakeK8sOps:    fake,
		deleteEntered: make(chan struct{}),
		deleteRelease: make(chan struct{}),
	}
	p := newProviderWithOps(ops)
	p.prebaked = true
	p.postStartSettle = 0
	p.runningPodCacheTTL = time.Hour
	if names, err := p.ListRunning(""); err != nil || len(names) != 0 {
		t.Fatalf("prime ListRunning = %v, %v; want empty", names, err)
	}
	hasSessionCalls := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			hasSessionCalls++
			if hasSessionCalls == 2 {
				return "", errors.New("session died after startup")
			}
		}
		return "", nil
	}

	startErr := make(chan error, 1)
	go func() {
		startErr <- p.Start(context.Background(), "worker", runtime.Config{
			Command:      "agent",
			ProcessNames: []string{"agent"},
			Env:          map[string]string{"GC_INSTANCE_TOKEN": "tok-worker"},
		})
	}()
	awaitSnapshotTestValue(t, ops.deleteEntered)
	names, err := p.ListRunning("")
	if err != nil || len(names) != 1 || names[0] != "worker" {
		t.Fatalf("ListRunning before cleanup delete commits = %v, %v; want stale [worker]", names, err)
	}
	close(ops.deleteRelease)
	if err := awaitSnapshotTestValue(t, startErr); !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Start error = %v, want ErrSessionDiedDuringStartup", err)
	}
	names, err = p.ListRunning("")
	if err != nil || len(names) != 0 {
		t.Fatalf("ListRunning after mutate-then-error cleanup delete = %v, %v; want empty", names, err)
	}
}

func TestStop(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Stop non-existent session is idempotent.
	if err := p.Stop("nonexistent"); err != nil {
		t.Fatalf("Stop non-existent: %v", err)
	}

	// Stop existing pod.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	if err := p.Stop("gc-test-agent"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify pod was deleted.
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Error("pod still exists after Stop")
	}
}

func TestStopInvalidatesRunningPodObservationSnapshot(t *testing.T) {
	pod := runningPodForSnapshot("pod-worker", "gc-city-worker")
	pod.UID = types.UID("uid-pod-worker")
	ops := newSnapshotListOps(pod)
	p := newProviderWithOps(ops)
	p.runningPodCacheTTL = time.Hour

	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("prime ListRunning: %v", err)
	}
	if len(names) != 1 || names[0] != "gc-city-worker" {
		t.Fatalf("prime ListRunning = %v, want [gc-city-worker]", names)
	}
	if err := p.Stop("gc-city-worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ops.pods = nil

	names, err = p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning after Stop: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("ListRunning after Stop = %v, want no sessions", names)
	}
	if got := ops.listCalls.Load(); got != 3 {
		t.Fatalf("Kubernetes LIST calls across prime, Stop, and post-Stop observation = %d, want 3", got)
	}
}

func TestListRunning(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Empty list.
	names, err := p.ListRunning("gc-test-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected 0 running, got %d", len(names))
	}

	// Add two running pods with annotations.
	addRunningPodWithAnnotation(fake, "gc-test-mayor", "gc-test-mayor", "gc-test-mayor")
	addRunningPodWithAnnotation(fake, "gc-test-polecat", "gc-test-polecat", "gc-test-polecat")
	addRunningPodWithAnnotation(fake, "gc-other-agent", "gc-other-agent", "gc-other-agent")

	names, err = p.ListRunning("gc-test-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 running with prefix, got %d: %v", len(names), names)
	}

	// Empty prefix returns all.
	names, err = p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning all: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("expected 3 running, got %d", len(names))
	}
}

func TestNudge(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	err := p.Nudge("gc-test-agent", runtime.TextContent("hello world"))
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	// Verify exec was called with literal mode:
	// Call 1: ["tmux", "send-keys", "-t", "main", "-l", "hello world"]
	// Call 2: ["tmux", "send-keys", "-t", "main", "Enter"]
	foundLiteral := false
	foundEnter := false
	for _, c := range fake.calls {
		if c.method != "execInPod" {
			continue
		}
		if len(c.cmd) >= 6 && c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" &&
			c.cmd[4] == "-l" && c.cmd[5] == "hello world" {
			foundLiteral = true
		}
		if len(c.cmd) >= 5 && c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" &&
			c.cmd[4] == "Enter" {
			foundEnter = true
		}
	}
	if !foundLiteral {
		t.Error("no tmux send-keys -l call recorded for Nudge")
	}
	if !foundEnter {
		t.Error("no tmux send-keys Enter call recorded for Nudge")
	}
}

func TestSendKeys(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	err := p.SendKeys("gc-test-agent", "Down", "Enter")
	if err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	// Verify the keys were passed to tmux.
	// Args: ["tmux", "send-keys", "-t", "main", "Down", "Enter"]
	found := false
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 6 {
			if c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" &&
				c.cmd[4] == "Down" && c.cmd[5] == "Enter" {
				found = true
			}
		}
	}
	if !found {
		t.Error("no tmux send-keys call with Down Enter")
	}
}

func TestInterrupt(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Interrupt non-existent session is best-effort.
	if err := p.Interrupt("nonexistent"); err != nil {
		t.Fatalf("Interrupt non-existent: %v", err)
	}

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	if err := p.Interrupt("gc-test-agent"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	// Verify C-c was sent.
	// Args: ["tmux", "send-keys", "-t", "main", "C-c"]
	found := false
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 5 {
			if c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" && c.cmd[4] == "C-c" {
				found = true
			}
		}
	}
	if !found {
		t.Error("no tmux send-keys C-c call recorded")
	}
}

func TestMetaOps(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	// SetMeta.
	if err := p.SetMeta("gc-test-agent", "GC_DRAIN", "true"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	// GetMeta — configure fake to return the value.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "show-environment", "-t", "main", "GC_DRAIN"},
		"GC_DRAIN=true\n", nil)

	val, err := p.GetMeta("gc-test-agent", "GC_DRAIN")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "true" {
		t.Errorf("GetMeta = %q, want %q", val, "true")
	}

	// GetMeta with unset key.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "show-environment", "-t", "main", "MISSING"},
		"-MISSING\n", nil)

	val, err = p.GetMeta("gc-test-agent", "MISSING")
	if err != nil {
		t.Fatalf("GetMeta unset: %v", err)
	}
	if val != "" {
		t.Errorf("GetMeta unset = %q, want empty", val)
	}

	// RemoveMeta.
	if err := p.RemoveMeta("gc-test-agent", "GC_DRAIN"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
}

func TestGetMetaInstanceTokenUsesImmutablePodSpecBeforeTmux(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.pods["gc-test-agent"].Status.Phase = corev1.PodPending
	fake.pods["gc-test-agent"].Spec.Containers = []corev1.Container{{
		Name: "agent",
		Env:  []corev1.EnvVar{{Name: "GC_INSTANCE_TOKEN", Value: "tok-immutable"}},
	}}
	fake.execFunc = func(string, []string) (string, error) {
		return "", errors.New("tmux is not ready")
	}

	got, err := p.GetMeta("gc-test-agent", "GC_INSTANCE_TOKEN")
	if err != nil {
		t.Fatalf("GetMeta(GC_INSTANCE_TOKEN): %v", err)
	}
	if got != "tok-immutable" {
		t.Fatalf("GetMeta(GC_INSTANCE_TOKEN) = %q, want tok-immutable", got)
	}
	for _, call := range fake.calls {
		if call.method == "execInPod" {
			t.Fatal("immutable instance-token lookup attempted a tmux exec")
		}
	}
}

func TestStopIfInstanceTokenPreservesSameNameReplacementCreatedAfterList(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	old := fake.pods["gc-test-agent"]
	old.UID = types.UID("uid-old")
	old.Spec.Containers = []corev1.Container{{
		Name: "agent",
		Env:  []corev1.EnvVar{{Name: "GC_INSTANCE_TOKEN", Value: "tok-old"}},
	}}
	fake.beforeDelete = func(name string) {
		fake.beforeDelete = nil
		fake.pods[name] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				UID:    types.UID("uid-replacement"),
				Labels: map[string]string{"app": "gc-agent", "gc-session": "gc-test-agent"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "agent",
				Env:  []corev1.EnvVar{{Name: "GC_INSTANCE_TOKEN", Value: "tok-replacement"}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}

	if err := p.StopIfInstanceToken("gc-test-agent", "tok-old"); err != nil {
		t.Fatalf("StopIfInstanceToken: %v", err)
	}
	got := fake.pods["gc-test-agent"]
	if got == nil || got.UID != "uid-replacement" {
		t.Fatalf("replacement pod = %#v, want uid-replacement preserved", got)
	}
	for _, call := range fake.calls {
		if call.method == "deletePod" && call.uid != "uid-old" {
			t.Fatalf("delete UID = %q, want only captured uid-old", call.uid)
		}
	}
}

func TestStopIfInstanceTokenDeletesOnlyMatchingPodsFromAmbiguousLabelSet(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "old-pod", token: "tok-old"},
		{name: "replacement-pod", token: "tok-replacement"},
	} {
		addRunningPod(fake, tc.name, "gc-test-agent")
		fake.pods[tc.name].Spec.Containers = []corev1.Container{{
			Name: "agent",
			Env:  []corev1.EnvVar{{Name: "GC_INSTANCE_TOKEN", Value: tc.token}},
		}}
	}

	if err := p.StopIfInstanceToken("gc-test-agent", "tok-old"); err != nil {
		t.Fatalf("StopIfInstanceToken: %v", err)
	}
	if _, exists := fake.pods["old-pod"]; exists {
		t.Fatal("token-matched old pod survived fenced stop")
	}
	if _, exists := fake.pods["replacement-pod"]; !exists {
		t.Fatal("different-token replacement pod was deleted")
	}
	deleteCalls := 0
	for _, call := range fake.calls {
		if call.method == "deletePod" {
			deleteCalls++
			if call.pod != "old-pod" {
				t.Fatalf("deleted pod %q, want only old-pod", call.pod)
			}
		}
	}
	if deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", deleteCalls)
	}
}

func TestGetMetaInstanceTokenRejectsDifferentTokensUnderOneSessionLabel(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	for _, tc := range []struct {
		name  string
		token string
	}{{name: "old-pod", token: "tok-old"}, {name: "new-pod", token: "tok-new"}} {
		addRunningPod(fake, tc.name, "gc-test-agent")
		fake.pods[tc.name].Spec.Containers = []corev1.Container{{Name: "agent", Env: []corev1.EnvVar{{Name: "GC_INSTANCE_TOKEN", Value: tc.token}}}}
	}
	if _, err := p.GetMeta("gc-test-agent", "GC_INSTANCE_TOKEN"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("GetMeta error = %v, want ErrRuntimeUnavailable for ambiguous pod identities", err)
	}
}

// deadlineCheckingOps wraps fakeK8sOps and fails listPods unless it is called
// with a tightly bounded context deadline. It mirrors the requireDeadline
// pattern used for the running-pod snapshot, applied here to prove the
// GC_INSTANCE_TOKEN probe — which runs inside the controller tick for every
// admitted journal — can never issue an unbounded LIST that would hang the
// whole controller loop on a wedged API server.
type deadlineCheckingOps struct {
	*fakeK8sOps
}

func (o *deadlineCheckingOps) listPods(ctx context.Context, selector, fieldSelector string) ([]corev1.Pod, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("GC_INSTANCE_TOKEN probe LIST has no deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 10*time.Second {
		return nil, fmt.Errorf("GC_INSTANCE_TOKEN probe LIST deadline is not tightly bounded: %s", remaining)
	}
	return o.fakeK8sOps.listPods(ctx, selector, fieldSelector)
}

// TestGetMetaInstanceTokenBoundsListPodsWithDeadline is the regression guard
// for the unbounded-LIST bug found reviewing PR #49 (gcy-bru): the immutable
// token probe calls listPods on the controller tick for every admitted
// journal. With no deadline, a wedged k8s API server hangs the entire
// controller. deadlineCheckingOps returns an error unless listPods is invoked
// with a tightly bounded context, so this test fails against the pre-fix
// context.Background() call and passes once the probe reuses
// runningPodSnapshotTimeout.
func TestGetMetaInstanceTokenBoundsListPodsWithDeadline(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.pods["gc-test-agent"].Status.Phase = corev1.PodPending
	fake.pods["gc-test-agent"].Spec.Containers = []corev1.Container{{
		Name: "agent",
		Env:  []corev1.EnvVar{{Name: "GC_INSTANCE_TOKEN", Value: "tok-immutable"}},
	}}
	p := newProviderWithOps(&deadlineCheckingOps{fakeK8sOps: fake})

	got, err := p.GetMeta("gc-test-agent", "GC_INSTANCE_TOKEN")
	if err != nil {
		t.Fatalf("GetMeta(GC_INSTANCE_TOKEN): %v", err)
	}
	if got != "tok-immutable" {
		t.Fatalf("GetMeta(GC_INSTANCE_TOKEN) = %q, want tok-immutable", got)
	}
}

// TestGetMetaInstanceTokenTimeoutIsFailClosed asserts that when the bounded
// token-probe LIST times out, GetMeta surfaces it as ErrRuntimeUnavailable
// (fail-closed) rather than a bare error: the async-start cleanup reconcile
// keys off ErrRuntimeUnavailable to preserve the journal instead of deleting.
func TestGetMetaInstanceTokenTimeoutIsFailClosed(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.listErr = context.DeadlineExceeded

	if _, err := p.GetMeta("gc-test-agent", "GC_INSTANCE_TOKEN"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("GetMeta timeout error = %v, want ErrRuntimeUnavailable (fail-closed)", err)
	}
}

func TestGetMetaFallsBackToInheritedGlobalEnvironment(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	sessionCmd := []string{"tmux", "show-environment", "-t", "main", "GC_SESSION_ID"}
	globalCmd := []string{"tmux", "show-environment", "-g", "GC_SESSION_ID"}
	fake.setExecResult("gc-test-agent", sessionCmd, "", errors.New("unknown variable: GC_SESSION_ID"))
	fake.setExecResult("gc-test-agent", globalCmd, "GC_SESSION_ID=st-123\n", nil)

	got, err := p.GetMeta("gc-test-agent", "GC_SESSION_ID")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got != "st-123" {
		t.Fatalf("GetMeta = %q, want st-123", got)
	}
}

func TestGetMetaInheritedVariableAbsent(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	sessionCmd := []string{"tmux", "show-environment", "-t", "main", "MISSING"}
	globalCmd := []string{"tmux", "show-environment", "-g", "MISSING"}
	fake.setExecResult("gc-test-agent", sessionCmd, "", errors.New("unknown variable: MISSING"))
	fake.setExecResult("gc-test-agent", globalCmd, "", errors.New("unknown variable: MISSING"))

	got, err := p.GetMeta("gc-test-agent", "MISSING")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got != "" {
		t.Fatalf("GetMeta = %q, want empty", got)
	}
}

func TestPeek(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	// Configure fake to return captured output.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "capture-pane", "-t", "main", "-p", "-S", "-50"},
		"line1\nline2\nline3\n", nil)

	output, err := p.Peek("gc-test-agent", 50)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if output != "line1\nline2\nline3\n" {
		t.Errorf("Peek output = %q, want lines", output)
	}
}

func TestGetLastActivity(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	// Configure fake to return epoch timestamp.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "display-message", "-t", "main", "-p", "#{session_activity}"},
		"1709300000\n", nil)

	activity, err := p.GetLastActivity("gc-test-agent")
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	want := time.Unix(1709300000, 0)
	if !activity.Equal(want) {
		t.Errorf("GetLastActivity = %v, want %v", activity, want)
	}

	// Non-existent session returns zero time.
	activity, err = p.GetLastActivity("nonexistent")
	if err != nil {
		t.Fatalf("GetLastActivity nonexistent: %v", err)
	}
	if !activity.IsZero() {
		t.Errorf("expected zero time, got %v", activity)
	}
}

func TestClearScrollback(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	addRunningPod(fake, "gc-test-agent", "gc-test-agent")

	if err := p.ClearScrollback("gc-test-agent"); err != nil {
		t.Fatalf("ClearScrollback: %v", err)
	}

	found := false
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 3 {
			if c.cmd[0] == "tmux" && c.cmd[1] == "clear-history" {
				found = true
			}
		}
	}
	if !found {
		t.Error("no tmux clear-history call recorded")
	}
}

func TestProcessAlive(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Empty process names → always true.
	if !p.ProcessAlive("any", nil) {
		t.Error("ProcessAlive with nil names should return true")
	}

	// No pod → false.
	if p.ProcessAlive("nonexistent", []string{"claude"}) {
		t.Error("ProcessAlive returned true for non-existent pod")
	}

	// Pod with process running.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent", []string{"pgrep", "-f", "claude"}, "1234\n", nil)

	if !p.ProcessAlive("gc-test-agent", []string{"claude"}) {
		t.Error("ProcessAlive returned false when process is running")
	}

	// Pod being deleted (has deletionTimestamp).
	now := metav1.Now()
	fake.pods["gc-test-agent"].DeletionTimestamp = &now

	if p.ProcessAlive("gc-test-agent", []string{"claude"}) {
		t.Error("ProcessAlive returned true for terminating pod")
	}
}

// TestProcessAliveCheckedDistinguishesConfirmedFromInconclusive is the
// ProcessAlive analogue of TestIsRunningCheckedDistinguishesConfirmedFromInconclusive.
func TestProcessAliveCheckedDistinguishesConfirmedFromInconclusive(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Confirmed negative: no pod at all.
	alive, err := p.ProcessAliveChecked("gc-test-agent", []string{"claude"})
	if alive {
		t.Error("ProcessAliveChecked reported alive with no pod")
	}
	if err != nil {
		t.Errorf("ProcessAliveChecked returned an error for a confirmed-absent pod: %v", err)
	}

	// Inconclusive: the pod lookup itself fails (e.g. a timed-out LIST).
	fake.listErr = context.DeadlineExceeded
	alive, err = p.ProcessAliveChecked("gc-test-agent", []string{"claude"})
	if alive {
		t.Error("ProcessAliveChecked reported alive despite a failed lookup")
	}
	if err == nil {
		t.Fatal("ProcessAliveChecked swallowed a lookup failure instead of reporting it as inconclusive")
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("ProcessAliveChecked error = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
	}
	if p.ProcessAlive("gc-test-agent", []string{"claude"}) {
		t.Error("ProcessAlive true despite a failed lookup")
	}
	fake.listErr = nil

	// Confirmed negative: pod exists, pgrep ran and confirmed the process
	// absent (an exit code, not a transport failure).
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent", []string{"pgrep", "-f", "claude"}, "",
		execerr.CodeExitError{Err: fmt.Errorf("exit status 1"), Code: 1})
	alive, err = p.ProcessAliveChecked("gc-test-agent", []string{"claude"})
	if alive {
		t.Error("ProcessAliveChecked reported alive for a confirmed exit-code result")
	}
	if err != nil {
		t.Errorf("ProcessAliveChecked returned an error for a confirmed exit-code result: %v", err)
	}

	// Inconclusive: exec itself fails to complete (transport), not a
	// command exit code.
	fake.setExecResult("gc-test-agent", []string{"pgrep", "-f", "claude"}, "",
		fmt.Errorf("stream error: broken pipe"))
	alive, err = p.ProcessAliveChecked("gc-test-agent", []string{"claude"})
	if alive {
		t.Error("ProcessAliveChecked reported alive despite an exec transport failure")
	}
	if err == nil {
		t.Fatal("ProcessAliveChecked swallowed an exec transport failure instead of reporting it as inconclusive")
	}
	if p.ProcessAlive("gc-test-agent", []string{"claude"}) {
		t.Error("ProcessAlive true despite an exec transport failure")
	}
}

func TestStartRequiresImage(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.image = "" // no image

	err := p.Start(context.Background(), "test", runtime.Config{})
	if err == nil {
		t.Fatal("Start should fail without image")
	}
	if want := "GC_K8S_IMAGE is required"; !contains(err.Error(), want) {
		t.Errorf("error = %q, want containing %q", err, want)
	}
}

func TestStartCreatesPodsAndWaits(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Configure fake to make tmux has-session succeed immediately.
	// The fake createPod sets phase=Running automatically.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command:      "claude --settings .gc/settings.json",
		ProcessNames: []string{"claude"},
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/workspace",
		},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify pod was created.
	if _, exists := fake.pods["gc-test-agent"]; !exists {
		t.Error("pod not created")
	}

	// Verify labels on the created pod.
	pod := fake.pods["gc-test-agent"]
	if pod.Labels["app"] != "gc-agent" {
		t.Errorf("label app = %q, want gc-agent", pod.Labels["app"])
	}
	if pod.Labels["gc-session"] != "gc-test-agent" {
		t.Errorf("label gc-session = %q, want gc-test-agent", pod.Labels["gc-session"])
	}
	if pod.Annotations["gc-session-name"] != "gc-test-agent" {
		t.Errorf("annotation gc-session-name = %q, want gc-test-agent", pod.Annotations["gc-session-name"])
	}
}

func TestStartDeletesPodWhenCityImportsCannotInitialize(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	fake.setExecResult(
		"gc-test-agent",
		[]string{"gc", "--city", "/workspace", "import", "install"},
		"",
		errors.New("locked import unavailable"),
	)

	err := p.Start(context.Background(), "gc-test-agent", runtime.Config{
		Command: "claude",
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/workspace",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "installing locked city imports") {
		t.Fatalf("Start error = %v, want locked import initialization failure", err)
	}
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Fatal("pod was not deleted after partial city initialization")
	}
	if command := findExecCmd(fake, "touch /workspace/.gc-workspace-ready"); command != nil {
		t.Fatalf("workspace gate released after failed city initialization: %v", command)
	}
}

func TestStartDetectsStalePod(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Add a stale pod in Failed phase. This avoids the tmux liveness check
	// (only done for Running pods) and goes straight to delete+recreate.
	fake.pods["gc-test-agent"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gc-test-agent",
			UID:    "stale-failed-uid",
			Labels: map[string]string{"app": "gc-agent", "gc-session": "gc-test-agent"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}

	// After deletion and recreation, tmux works.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/workspace",
		},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start with stale pod: %v", err)
	}

	// Verify deletePod was called (to remove stale pod).
	found := false
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "gc-test-agent" {
			found = true
		}
	}
	if !found {
		t.Error("stale pod was not deleted before recreation")
	}
}

func TestStartRejectsExistingLiveSession(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Pre-existing pod with live tmux.
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env:          map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err == nil {
		t.Fatal("Start should fail for existing live session")
	}
	if want := "already exists"; !contains(err.Error(), want) {
		t.Errorf("error = %q, want containing %q", err, want)
	}
}

func TestStartTreatsYoungPodWithDeadTmuxAsInitializing(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Pod created recently — still within startup grace period.
	fake.pods["gc-test-agent"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "gc-test-agent",
			UID:               "young-running-uid",
			Labels:            map[string]string{"app": "gc-agent", "gc-session": "gc-test-agent"},
			CreationTimestamp: metav1.Now(),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// tmux not up yet (workspace init still blocking).
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "",
		fmt.Errorf("no server running on /tmp/tmux-1000/default"))

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env:          map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/workspace"},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err == nil {
		t.Fatal("Start should return error for initializing pod")
	}
	if !errors.Is(err, runtime.ErrSessionInitializing) {
		t.Errorf("error = %v, want ErrSessionInitializing", err)
	}

	// Must NOT have deleted the pod — it's still initializing.
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "gc-test-agent" {
			t.Error("young pod was deleted despite still initializing")
		}
	}
}

func TestStartDeletesOldPodWithDeadTmux(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)

	// Pod created long ago — well past the startup grace period.
	fake.pods["gc-test-agent"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "gc-test-agent",
			UID:               "old-running-uid",
			Labels:            map[string]string{"app": "gc-agent", "gc-session": "gc-test-agent"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// tmux dead — genuinely stale.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "",
		fmt.Errorf("no server running on /tmp/tmux-1000/default"))

	// Block createPod so Start() stops after deletion — we only need to
	// verify the stale pod was cleaned up, not the full startup.
	fake.createErr = fmt.Errorf("intentional: verify deletion only")

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/workspace",
		},
	}
	_ = p.Start(context.Background(), "gc-test-agent", cfg)

	// Must have deleted the stale pod.
	found := false
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "gc-test-agent" {
			found = true
		}
	}
	if !found {
		t.Error("old stale pod was not deleted before recreation")
	}
}

func TestPodManifestCompatibility(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/demo-rig",
		Env: map[string]string{
			"GC_AGENT": "demo-rig/polecat",
			"GC_CITY":  "/city",
		},
	}

	pod, err := buildPod("gc-bright-demo-rig-polecat", cfg, p)
	if err != nil {
		t.Fatal(err)
	}

	// Container name must be "agent".
	if pod.Spec.Containers[0].Name != "agent" {
		t.Errorf("container name = %q, want %q", pod.Spec.Containers[0].Name, "agent")
	}

	// Init container name must be "stage" (when staging needed).
	if len(pod.Spec.InitContainers) == 0 {
		t.Fatal("expected init container for rig agent")
	}
	if pod.Spec.InitContainers[0].Name != "stage" {
		t.Errorf("init container name = %q, want %q", pod.Spec.InitContainers[0].Name, "stage")
	}

	// Labels must match gc-session-k8s format.
	if pod.Labels["app"] != "gc-agent" {
		t.Errorf("label app = %q, want gc-agent", pod.Labels["app"])
	}

	// Verify volume names.
	volNames := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		volNames[v.Name] = true
	}
	for _, name := range []string{"ws", "claude-config", "city"} {
		if !volNames[name] {
			t.Errorf("missing volume %q", name)
		}
	}

	// Verify working directory is pod-mapped.
	if pod.Spec.Containers[0].WorkingDir != "/workspace/demo-rig" {
		t.Errorf("workingDir = %q, want /workspace/demo-rig",
			pod.Spec.Containers[0].WorkingDir)
	}
}

func TestWorkspaceVolumeMountsAtRoot(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())

	tests := []struct {
		name    string
		workDir string
	}{
		{"default workspace", "/city"},
		{"rig subdirectory", "/city/demo-rig"},
		{"deep gc subdirectory", "/city/.gc/agents/deacon"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := runtime.Config{
				Command: "claude",
				WorkDir: tt.workDir,
				Env: map[string]string{
					"GC_AGENT": "test/agent",
					"GC_CITY":  "/city",
				},
			}

			pod, err := buildPod("gc-test-agent", cfg, p)
			if err != nil {
				t.Fatal(err)
			}

			for _, vm := range pod.Spec.Containers[0].VolumeMounts {
				if vm.Name == "ws" {
					if vm.MountPath != "/workspace" {
						t.Errorf("ws volume MountPath = %q, want /workspace", vm.MountPath)
					}
					return
				}
			}
			// ws volume not found — only expected for prebaked
			if !p.prebaked {
				t.Error("ws volume mount not found on agent container")
			}
		})
	}
}

func mustBuildPodEnv(t *testing.T, cfgEnv map[string]string, podWorkDir, managedServiceHost, managedServicePort string) []corev1.EnvVar {
	t.Helper()
	env, err := buildPodEnv(cfgEnv, cfgEnv["GC_DIR"], podWorkDir, managedServiceHost, managedServicePort)
	if err != nil {
		t.Fatalf("buildPodEnv: %v", err)
	}
	return env
}

func TestBuildPodEnvRemapsVars(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":                            "mayor",
		"GC_CITY":                             "/host/city",
		"GC_CITY_PATH":                        "/host/city",
		"GC_DIR":                              "/host/city/rig",
		"GC_RIG_ROOT":                         "/host/city/rig",
		"GC_STORE_ROOT":                       "/host/city/rig",
		"BEADS_DIR":                           "/host/city/rig/.beads",
		"GT_ROOT":                             "/host/city",
		"GC_CITY_RUNTIME_DIR":                 "/host/city/.gc/runtime",
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": "/host/city/.gc/runtime/control-dispatcher-trace.log",
		"GC_PACK_STATE_DIR":                   "/host/city/.gc/runtime/packs/rlm",
		"GC_PACK_DIR":                         "/host/city/packs/maintenance",
		"GC_SESSION":                          "exec:gc-session-k8s",
		"GC_BEADS":                            "exec:something",
		"GC_EVENTS":                           "exec:other",
		"GC_DOLT_HOST":                        "",
		"GC_DOLT_PORT":                        "3307",
		"BEADS_DOLT_SERVER_HOST":              "",
		"BEADS_DOLT_SERVER_PORT":              "3307",
		"GC_K8S_DOLT_HOST":                    "legacy-dolt.example.com",
		"GC_K8S_DOLT_PORT":                    "3308",
		"GC_K8S_DOLT_SECRET":                  "dolt-credentials",
		"GC_DOLT_USER":                        "admin",
		"GC_DOLT_PASSWORD":                    "secret",
		"BEADS_DOLT_SERVER_USER":              "admin",
		"BEADS_DOLT_PASSWORD":                 "secret",
		"GC_MAIL":                             "exec:mail",
		"GC_MCP_MAIL_URL":                     "http://localhost:8765",
		"CUSTOM_VAR":                          "preserved",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace/rig", podManagedDoltHost, podManagedDoltPort)

	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	// GC_CITY should be remapped to /workspace.
	if envMap["GC_CITY"] != "/workspace" {
		t.Errorf("GC_CITY = %q, want /workspace", envMap["GC_CITY"])
	}
	if envMap["GC_CITY_PATH"] != "/workspace" {
		t.Errorf("GC_CITY_PATH = %q, want /workspace", envMap["GC_CITY_PATH"])
	}

	// GC_DIR should be remapped to pod work dir.
	if envMap["GC_DIR"] != "/workspace/rig" {
		t.Errorf("GC_DIR = %q, want /workspace/rig", envMap["GC_DIR"])
	}

	// GC_RIG_ROOT should be remapped from controller city path to /workspace.
	if envMap["GC_RIG_ROOT"] != "/workspace/rig" {
		t.Errorf("GC_RIG_ROOT = %q, want /workspace/rig", envMap["GC_RIG_ROOT"])
	}

	// GC_STORE_ROOT should be remapped from controller city path to /workspace.
	if envMap["GC_STORE_ROOT"] != "/workspace/rig" {
		t.Errorf("GC_STORE_ROOT = %q, want /workspace/rig", envMap["GC_STORE_ROOT"])
	}

	// BEADS_DIR should be remapped from controller city path to /workspace.
	if envMap["BEADS_DIR"] != "/workspace/rig/.beads" {
		t.Errorf("BEADS_DIR = %q, want /workspace/rig/.beads", envMap["BEADS_DIR"])
	}

	// GT_ROOT should be remapped from controller city path to /workspace.
	if envMap["GT_ROOT"] != "/workspace" {
		t.Errorf("GT_ROOT = %q, want /workspace", envMap["GT_ROOT"])
	}

	// GC_CITY_RUNTIME_DIR should be remapped.
	if envMap["GC_CITY_RUNTIME_DIR"] != "/workspace/.gc/runtime" {
		t.Errorf("GC_CITY_RUNTIME_DIR = %q, want /workspace/.gc/runtime", envMap["GC_CITY_RUNTIME_DIR"])
	}

	// GC_CONTROL_DISPATCHER_TRACE_DEFAULT should be remapped.
	if envMap["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != "/workspace/.gc/runtime/control-dispatcher-trace.log" {
		t.Errorf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want /workspace/.gc/runtime/control-dispatcher-trace.log", envMap["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"])
	}

	// GC_PACK_STATE_DIR should be remapped.
	if envMap["GC_PACK_STATE_DIR"] != "/workspace/.gc/runtime/packs/rlm" {
		t.Errorf("GC_PACK_STATE_DIR = %q, want /workspace/.gc/runtime/packs/rlm", envMap["GC_PACK_STATE_DIR"])
	}

	// GC_PACK_DIR should be remapped.
	if envMap["GC_PACK_DIR"] != "/workspace/packs/maintenance" {
		t.Errorf("GC_PACK_DIR = %q, want /workspace/packs/maintenance", envMap["GC_PACK_DIR"])
	}

	// Controller-only vars should be removed. The pod adapter reprojects the
	// canonical GC target and derives the BEADS host/port mirror from it.
	for _, key := range []string{"GC_SESSION", "GC_BEADS", "GC_EVENTS", "GC_K8S_DOLT_HOST", "GC_K8S_DOLT_PORT", "GC_K8S_DOLT_SECRET"} {
		if _, exists := envMap[key]; exists {
			t.Errorf("controller-only var %s should be removed", key)
		}
	}
	// Canonical Dolt connection vars should remain present, and local/controller
	// endpoints should be reprojected to the in-cluster managed service target.
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "BEADS_DOLT_SERVER_USER", "BEADS_DOLT_PASSWORD"} {
		if _, exists := envMap[key]; !exists {
			t.Errorf("connection var %s should be preserved in agent pods", key)
		}
	}
	if envMap["GC_DOLT_HOST"] != podManagedDoltHost {
		t.Errorf("GC_DOLT_HOST = %q, want %q", envMap["GC_DOLT_HOST"], podManagedDoltHost)
	}
	if envMap["GC_DOLT_PORT"] != podManagedDoltPort {
		t.Errorf("GC_DOLT_PORT = %q, want %q", envMap["GC_DOLT_PORT"], podManagedDoltPort)
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != podManagedDoltHost {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", envMap["BEADS_DOLT_SERVER_HOST"], podManagedDoltHost)
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != podManagedDoltPort {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", envMap["BEADS_DOLT_SERVER_PORT"], podManagedDoltPort)
	}

	// Mail vars should be passed through to agent pods.
	if envMap["GC_MAIL"] != "exec:mail" {
		t.Errorf("GC_MAIL = %q, want exec:mail", envMap["GC_MAIL"])
	}
	if envMap["GC_MCP_MAIL_URL"] != "http://localhost:8765" {
		t.Errorf("GC_MCP_MAIL_URL = %q, want http://localhost:8765", envMap["GC_MCP_MAIL_URL"])
	}

	// Custom vars should be preserved.
	if envMap["CUSTOM_VAR"] != "preserved" {
		t.Errorf("CUSTOM_VAR = %q, want preserved", envMap["CUSTOM_VAR"])
	}

	// GC_TMUX_SESSION should be added.
	if envMap["GC_TMUX_SESSION"] != "main" {
		t.Errorf("GC_TMUX_SESSION = %q, want main", envMap["GC_TMUX_SESSION"])
	}
}

func TestBuildPodEnvReprojectsExternalRuntimeRoots(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_CITY":                             "/host/city",
		"GC_CITY_PATH":                        "/host/city",
		"GC_CITY_RUNTIME_DIR":                 "/var/tmp/gascity-runtime",
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": "/var/tmp/gascity-runtime/control-dispatcher-trace.log",
		"GC_PACK_STATE_DIR":                   "/var/tmp/gascity-runtime/packs/rlm",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort)

	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_CITY_RUNTIME_DIR"] != "/workspace/.gc/runtime" {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want /workspace/.gc/runtime", envMap["GC_CITY_RUNTIME_DIR"])
	}
	if envMap["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] != "/workspace/.gc/runtime/control-dispatcher-trace.log" {
		t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want /workspace/.gc/runtime/control-dispatcher-trace.log", envMap["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"])
	}
	if envMap["GC_PACK_STATE_DIR"] != "/workspace/.gc/runtime/packs/rlm" {
		t.Fatalf("GC_PACK_STATE_DIR = %q, want /workspace/.gc/runtime/packs/rlm", envMap["GC_PACK_STATE_DIR"])
	}
}

func TestBuildPodEnvProjectsManagedDoltEndpoint(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":               "worker",
		"GC_DOLT_HOST":           "",
		"GC_DOLT_PORT":           "4123",
		"BEADS_DOLT_SERVER_HOST": "",
		"BEADS_DOLT_SERVER_PORT": "4123",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort)
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != podManagedDoltHost {
		t.Errorf("GC_DOLT_HOST = %q, want %q", envMap["GC_DOLT_HOST"], podManagedDoltHost)
	}
	if envMap["GC_DOLT_PORT"] != podManagedDoltPort {
		t.Errorf("GC_DOLT_PORT = %q, want %q", envMap["GC_DOLT_PORT"], podManagedDoltPort)
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != podManagedDoltHost {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", envMap["BEADS_DOLT_SERVER_HOST"], podManagedDoltHost)
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != podManagedDoltPort {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", envMap["BEADS_DOLT_SERVER_PORT"], podManagedDoltPort)
	}
}

func TestBuildPodEnvProjectsManagedLocalDoltTarget(t *testing.T) {
	env := mustBuildPodEnv(t, map[string]string{
		"GC_AGENT":         "worker",
		"GC_DOLT_PORT":     "31364",
		"GC_K8S_DOLT_HOST": "legacy-dolt.example.com",
		"GC_K8S_DOLT_PORT": "3309",
	}, "/workspace", podManagedDoltHost, podManagedDoltPort)

	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != podManagedDoltHost {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", envMap["GC_DOLT_HOST"], podManagedDoltHost)
	}
	if envMap["GC_DOLT_PORT"] != podManagedDoltPort {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", envMap["GC_DOLT_PORT"], podManagedDoltPort)
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != podManagedDoltHost {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want %q", envMap["BEADS_DOLT_SERVER_HOST"], podManagedDoltHost)
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != podManagedDoltPort {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", envMap["BEADS_DOLT_SERVER_PORT"], podManagedDoltPort)
	}
}

func TestBuildPodEnvRejectsHostOnlyProjectedTarget(t *testing.T) {
	_, err := buildPodEnv(map[string]string{
		"GC_AGENT":     "worker",
		"GC_DOLT_HOST": "canonical-dolt.example.com",
	}, "", "/workspace", podManagedDoltHost, podManagedDoltPort)
	if err == nil {
		t.Fatal("expected host-only GC_DOLT_* projection to fail")
	}
	if got := err.Error(); got != "requires both GC_DOLT_HOST and GC_DOLT_PORT when GC_DOLT_HOST is set" {
		t.Fatalf("buildPodEnv error = %q", got)
	}
}

func TestBuildPodEnvPreservesExplicitDoltVars(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":               "worker",
		"GC_DOLT_HOST":           "custom-dolt.example.com",
		"GC_DOLT_PORT":           "3308",
		"BEADS_DOLT_SERVER_HOST": "custom-dolt.example.com",
		"BEADS_DOLT_SERVER_PORT": "3308",
		"GC_K8S_DOLT_HOST":       "legacy-dolt.example.com",
		"GC_K8S_DOLT_PORT":       "3309",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort)

	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	// Explicit canonical values should pass through unchanged and the legacy
	// K8s-only aliases should be stripped.
	if envMap["GC_DOLT_HOST"] != "custom-dolt.example.com" {
		t.Errorf("GC_DOLT_HOST = %q, want custom-dolt.example.com", envMap["GC_DOLT_HOST"])
	}
	if envMap["GC_DOLT_PORT"] != "3308" {
		t.Errorf("GC_DOLT_PORT = %q, want 3308", envMap["GC_DOLT_PORT"])
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != "custom-dolt.example.com" {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want custom-dolt.example.com", envMap["BEADS_DOLT_SERVER_HOST"])
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != "3308" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want 3308", envMap["BEADS_DOLT_SERVER_PORT"])
	}
	if _, exists := envMap["GC_K8S_DOLT_HOST"]; exists {
		t.Error("GC_K8S_DOLT_HOST should be stripped")
	}
	if _, exists := envMap["GC_K8S_DOLT_PORT"]; exists {
		t.Error("GC_K8S_DOLT_PORT should be stripped")
	}
}

func TestBuildPodEnvMirrorsBeadsEndpointFromProjectedGCDoltVars(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":               "worker",
		"GC_DOLT_HOST":           "canonical-dolt.example.com",
		"GC_DOLT_PORT":           "3308",
		"BEADS_DOLT_SERVER_HOST": "stale-beads.example.com",
		"BEADS_DOLT_SERVER_PORT": "9911",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", podManagedDoltHost, podManagedDoltPort)
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != "canonical-dolt.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical-dolt.example.com", envMap["GC_DOLT_HOST"])
	}
	if envMap["GC_DOLT_PORT"] != "3308" {
		t.Fatalf("GC_DOLT_PORT = %q, want 3308", envMap["GC_DOLT_PORT"])
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != "canonical-dolt.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want mirrored canonical host", envMap["BEADS_DOLT_SERVER_HOST"])
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != "3308" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want mirrored canonical port", envMap["BEADS_DOLT_SERVER_PORT"])
	}
}

func TestBuildPodEnvUsesProviderManagedAlias(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":     "worker",
		"GC_DOLT_PORT": "31364",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", "pod-dolt.internal", "4407")
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != "pod-dolt.internal" {
		t.Fatalf("GC_DOLT_HOST = %q, want pod-dolt.internal", envMap["GC_DOLT_HOST"])
	}
	if envMap["GC_DOLT_PORT"] != "4407" {
		t.Fatalf("GC_DOLT_PORT = %q, want 4407", envMap["GC_DOLT_PORT"])
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != "pod-dolt.internal" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want pod-dolt.internal", envMap["BEADS_DOLT_SERVER_HOST"])
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != "4407" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want 4407", envMap["BEADS_DOLT_SERVER_PORT"])
	}
}

func TestBuildPodEnvRemapsLoopbackDoltTargetToManagedService(t *testing.T) {
	cfgEnv := map[string]string{
		"GC_AGENT":     "worker",
		"GC_DOLT_HOST": "127.0.0.1",
		"GC_DOLT_PORT": "3308",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace", "pod-dolt.internal", "4407")
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_DOLT_HOST"] != "pod-dolt.internal" {
		t.Fatalf("GC_DOLT_HOST = %q, want pod-dolt.internal", envMap["GC_DOLT_HOST"])
	}
	if envMap["GC_DOLT_PORT"] != "4407" {
		t.Fatalf("GC_DOLT_PORT = %q, want 4407", envMap["GC_DOLT_PORT"])
	}
	if envMap["BEADS_DOLT_SERVER_HOST"] != "pod-dolt.internal" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want pod-dolt.internal", envMap["BEADS_DOLT_SERVER_HOST"])
	}
	if envMap["BEADS_DOLT_SERVER_PORT"] != "4407" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want 4407", envMap["BEADS_DOLT_SERVER_PORT"])
	}
}

func TestBuildPodEnvFallbackCityPath(t *testing.T) {
	// When GC_CITY is absent, the remap should fall back to GC_CITY_PATH.
	cfgEnv := map[string]string{
		"GC_CITY_PATH": "/host/city",
		"GC_RIG_ROOT":  "/host/city/rig",
		"BEADS_DIR":    "/host/city/rig/.beads",
		"GT_ROOT":      "/host/city",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace/rig", podManagedDoltHost, podManagedDoltPort)
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_RIG_ROOT"] != "/workspace/rig" {
		t.Errorf("GC_RIG_ROOT = %q, want /workspace/rig", envMap["GC_RIG_ROOT"])
	}
	if envMap["BEADS_DIR"] != "/workspace/rig/.beads" {
		t.Errorf("BEADS_DIR = %q, want /workspace/rig/.beads", envMap["BEADS_DIR"])
	}
	if envMap["GT_ROOT"] != "/workspace" {
		t.Errorf("GT_ROOT = %q, want /workspace", envMap["GT_ROOT"])
	}
}

func TestBuildPodEnvFallbackCityRoot(t *testing.T) {
	// When both GC_CITY and GC_CITY_PATH are absent, fall back to GC_CITY_ROOT.
	cfgEnv := map[string]string{
		"GC_CITY_ROOT": "/host/city",
		"GC_RIG_ROOT":  "/host/city/rig",
		"BEADS_DIR":    "/host/city/rig/.beads",
	}

	env := mustBuildPodEnv(t, cfgEnv, "/workspace/rig", podManagedDoltHost, podManagedDoltPort)
	envMap := map[string]string{}
	for _, e := range env {
		envMap[e.Name] = e.Value
	}

	if envMap["GC_RIG_ROOT"] != "/workspace/rig" {
		t.Errorf("GC_RIG_ROOT = %q, want /workspace/rig", envMap["GC_RIG_ROOT"])
	}
	if envMap["BEADS_DIR"] != "/workspace/rig/.beads" {
		t.Errorf("BEADS_DIR = %q, want /workspace/rig/.beads", envMap["BEADS_DIR"])
	}
}

func TestNeedsStaging(t *testing.T) {
	tests := []struct {
		name     string
		cfg      runtime.Config
		ctrlCity string
		want     bool
	}{
		{
			name:     "no staging",
			cfg:      runtime.Config{WorkDir: "/workspace"},
			ctrlCity: "/workspace",
			want:     false,
		},
		{
			name: "overlay dir",
			cfg:  runtime.Config{OverlayDir: "/some/overlay"},
			want: true,
		},
		{
			name:     "pack overlay dir",
			cfg:      runtime.Config{WorkDir: "/city", PackOverlayDirs: []string{"/some/pack"}},
			ctrlCity: "/city",
			want:     true,
		},
		{
			name: "copy files",
			cfg:  runtime.Config{CopyFiles: []runtime.CopyEntry{{Src: "/a"}}},
			want: true,
		},
		{
			name:     "rig agent (different work_dir)",
			cfg:      runtime.Config{WorkDir: "/city/rig"},
			ctrlCity: "/city",
			want:     true,
		},
		{
			name:     "city agent (same work_dir)",
			cfg:      runtime.Config{WorkDir: "/city"},
			ctrlCity: "/city",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsStaging(tt.cfg, tt.ctrlCity)
			if got != tt.want {
				t.Errorf("needsStaging = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodManifestAddsInitContainerForPackOverlayCityAgent(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())

	cfg := runtime.Config{
		Command:         "kiro-cli chat --no-interactive --agent gascity",
		WorkDir:         "/city",
		ProviderName:    "kiro",
		PackOverlayDirs: []string{"/packs/core/overlay"},
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/city",
		},
	}

	pod, err := buildPod("gc-city-mayor", cfg, p)
	if err != nil {
		t.Fatal(err)
	}

	if len(pod.Spec.InitContainers) == 0 {
		t.Fatal("expected init container for city agent with pack overlay")
	}
	if pod.Spec.InitContainers[0].Name != "stage" {
		t.Errorf("init container name = %q, want %q", pod.Spec.InitContainers[0].Name, "stage")
	}
}

func TestBuildPodPrebaked(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.prebaked = true

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/demo-rig",
		Env: map[string]string{
			"GC_AGENT": "demo-rig/polecat",
			"GC_CITY":  "/city",
		},
		OverlayDir: "/some/overlay", // would normally trigger staging
	}

	pod, err := buildPod("gc-bright-demo-rig-polecat", cfg, p)
	if err != nil {
		t.Fatal(err)
	}

	// No init containers when prebaked.
	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("expected 0 init containers when prebaked, got %d", len(pod.Spec.InitContainers))
	}

	// No "ws" EmptyDir volume.
	for _, v := range pod.Spec.Volumes {
		if v.Name == "ws" {
			t.Error("prebaked pod should not have 'ws' EmptyDir volume")
		}
		if v.Name == "city" {
			t.Error("prebaked pod should not have 'city' EmptyDir volume")
		}
	}

	// No "ws" volume mount on main container.
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "ws" {
			t.Error("prebaked pod should not have 'ws' volume mount")
		}
	}

	// claude-config Secret volume must still be present.
	hasClaudeConfig := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "claude-config" {
			hasClaudeConfig = true
		}
	}
	if !hasClaudeConfig {
		t.Error("prebaked pod missing claude-config Secret volume")
	}

	// Entrypoint should NOT contain workspace-ready wait.
	entrypoint := pod.Spec.Containers[0].Args[0]
	if containsStr(entrypoint, ".gc-workspace-ready") {
		t.Error("prebaked entrypoint should not wait for .gc-workspace-ready")
	}
}

func TestInitBeadsInPodUsesProjectedStoreRootAndPrefix(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		WorkDir: "/host/city/rigs/frontend",
		Env: map[string]string{
			"GC_CITY":         "/host/city",
			"GC_STORE_ROOT":   "/host/city/custom-scope",
			"GC_BEADS_PREFIX": "cs",
			"GC_DOLT_HOST":    "canonical-dolt.example.com",
			"GC_DOLT_PORT":    "3308",
		},
	}
	podWorkDir := projectedPodWorkDir(cfg)
	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, podWorkDir, podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("initBeadsInPod: %v", err)
	}
	wantStoreRootB64 := base64.StdEncoding.EncodeToString([]byte("/workspace/custom-scope"))
	wantPrefixB64 := base64.StdEncoding.EncodeToString([]byte("cs"))
	wrongWorkDirB64 := base64.StdEncoding.EncodeToString([]byte("/workspace/rigs/frontend"))
	found := false
	for _, c := range fake.calls {
		if c.method != "execInPod" || len(c.cmd) < 3 {
			continue
		}
		if c.cmd[0] != "sh" || c.cmd[1] != "-c" {
			continue
		}
		script := c.cmd[2]
		if !strings.Contains(script, wantStoreRootB64) || !strings.Contains(script, wantPrefixB64) {
			continue
		}
		if strings.Contains(script, wrongWorkDirB64) {
			t.Fatalf("repair script used pod workdir instead of projected store root: %s", script)
		}
		if strings.Contains(script, "m.pop('project_id'") {
			t.Fatalf("repair script stripped canonical project_id: %s", script)
		}
		found = true
	}
	if !found {
		t.Fatal("initBeadsInPod did not use projected store root and prefix")
	}
}

func TestVerifyBeadsInPodChecksCanonicalFiles(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_STORE_ROOT": "/host/city/frontend",
			"GC_DOLT_HOST":  "dolt.gc.svc.cluster.local",
			"GC_DOLT_PORT":  "3307",
		},
	}

	if err := verifyBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/frontend", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("verifyBeadsInPod: %v", err)
	}

	found := false
	for _, c := range fake.calls {
		if c.method != "execInPod" || len(c.cmd) < 5 {
			continue
		}
		if c.cmd[0] != "sh" || c.cmd[1] != "-c" {
			continue
		}
		script := c.cmd[2]
		if containsStr(script, "test -f .beads/metadata.json") &&
			containsStr(script, "test -f .beads/config.yaml") &&
			!containsStr(script, "bd init") &&
			c.cmd[4] == "/workspace/frontend" {
			found = true
		}
	}
	if !found {
		t.Fatal("verifyBeadsInPod did not check canonical .beads files with the expected workdir")
	}
}

func TestVerifyBeadsInPodRunsForManagedProjection(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_DOLT_PORT": "31364",
		},
	}

	if err := verifyBeadsInPod(context.Background(), fake, "test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("verifyBeadsInPod() error = %v", err)
	}
	if len(fake.calls) == 0 {
		t.Fatal("expected managed projection to trigger canonical .beads verification")
	}
}

func TestVerifyBeadsInPodSkipsWithoutProjectedTarget(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{Env: map[string]string{}}

	if err := verifyBeadsInPod(context.Background(), fake, "test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("verifyBeadsInPod() error = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("expected no pod exec calls without a projected Dolt target, got %d", len(fake.calls))
	}
}

func TestVerifyBeadsInPodRejectsHostOnlyProjectedTarget(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_DOLT_HOST": "canonical-dolt.example.com",
		},
	}

	err := verifyBeadsInPod(context.Background(), fake, "test-pod", cfg, "/workspace/frontend", podManagedDoltHost, podManagedDoltPort)
	if err == nil {
		t.Fatal("expected host-only GC_DOLT_* projection to fail")
	}
	if got := err.Error(); got != "requires both GC_DOLT_HOST and GC_DOLT_PORT when GC_DOLT_HOST is set" {
		t.Fatalf("verifyBeadsInPod error = %q", got)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("expected no pod exec calls after invalid projected target, got %d", len(fake.calls))
	}
}

func TestStartUsesPodBeadsRepairScript(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.prebaked = true
	p.postStartSettle = 0

	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/rig",
		Env: map[string]string{
			"GC_AGENT":        "rig/polecat",
			"GC_CITY":         "/city",
			"GC_STORE_ROOT":   "/city/custom-scope",
			"GC_BEADS_PREFIX": "cs",
			"GC_DOLT_PORT":    "31364",
		},
	}
	if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	foundRepair := false
	for _, c := range fake.calls {
		if c.method != "execInPod" || len(c.cmd) < 3 {
			continue
		}
		if c.cmd[0] != "sh" || c.cmd[1] != "-c" {
			continue
		}
		script := c.cmd[2]
		if containsStr(script, "bd init --server") && containsStr(script, "m.update(p)") {
			foundRepair = true
			break
		}
	}
	if !foundRepair {
		t.Fatal("Start did not invoke the pod .beads repair/bootstrap script")
	}
}

func TestStartWarnsWhenInitBeadsInPodFails(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.prebaked = true
	p.postStartSettle = 0

	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "sh" && cmd[1] == "-c" && containsStr(cmd[2], "bd init --server") {
			return "", errors.New("missing canonical beads")
		}
		return "", nil
	}
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/rig",
		Env: map[string]string{
			"GC_AGENT":     "rig/polecat",
			"GC_CITY":      "/city",
			"GC_DOLT_PORT": "31364",
		},
	}
	if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
		t.Fatalf("Start should warn and continue when pod beads repair fails: %v", err)
	}
}

// TestInitBeadsInPodBdInitSetsBEADSDIR verifies that the pod bootstrap bd init
// sets BEADS_DIR so bd does not create a .git/ as a side effect in the pod
// workspace. Regression for #399.
func TestInitBeadsInPodBdInitSetsBEADSDIR(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_DOLT_HOST":    podManagedDoltHost,
			"GC_DOLT_PORT":    podManagedDoltPort,
			"GC_BEADS_PREFIX": "demo",
		},
	}
	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("initBeadsInPod: %v", err)
	}
	var script string
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 3 && c.cmd[0] == "sh" && c.cmd[1] == "-c" {
			script = c.cmd[2]
			break
		}
	}
	if script == "" {
		t.Fatal("no sh -c exec call found")
	}
	want := `BEADS_DIR="$WD/.beads" bd init --server`
	if !strings.Contains(script, want) {
		t.Errorf("bd init invocation missing BEADS_DIR env prefix: %q not found in script:\n%s", want, script)
	}
}

// TestInitBeadsInPodPreservesCanonicalProjectID verifies that endpoint
// projection does not discard the database identity already validated by the
// controller. Native store preflight requires this L1 identity in the pod.
func TestInitBeadsInPodPreservesCanonicalProjectID(t *testing.T) {
	fake := newFakeK8sOps()
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_DOLT_HOST":    podManagedDoltHost,
			"GC_DOLT_PORT":    podManagedDoltPort,
			"GC_BEADS_PREFIX": "demo",
		},
	}

	if err := initBeadsInPod(context.Background(), fake, "gc-test-pod", cfg, "/workspace/demo-repo", podManagedDoltHost, podManagedDoltPort); err != nil {
		t.Fatalf("initBeadsInPod: %v", err)
	}

	var script string
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 3 && c.cmd[0] == "sh" && c.cmd[1] == "-c" {
			script = c.cmd[2]
			break
		}
	}
	if script == "" {
		t.Fatal("no sh -c exec call found")
	}

	if strings.Contains(script, "m.pop('project_id'") {
		t.Errorf("metadata patch must preserve canonical project_id:\n%s", script)
	}
	if strings.Contains(script, "<<<") {
		t.Errorf("metadata patch script must be POSIX sh compatible; found bash here-string in:\n%s", script)
	}
	if !strings.Contains(script, `printf '%s' "$PATCH" | python3 -c`) {
		t.Errorf("metadata patch fallback should pipe PATCH into python3 stdin for POSIX sh compatibility:\n%s", script)
	}
}

func TestStartSkipsStagingWhenPrebaked(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.prebaked = true

	// Configure fake so tmux check succeeds.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		WorkDir: "/city/rig",
		Env: map[string]string{
			"GC_AGENT": "rig/polecat",
			"GC_CITY":  "/city",
		},
		OverlayDir: "/some/overlay",
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start prebaked: %v", err)
	}

	// Verify no staging-related exec calls occurred.
	for _, c := range fake.calls {
		if c.method == "execInPod" {
			// Should not see touch .gc-workspace-ready
			if len(c.cmd) >= 2 && c.cmd[0] == "touch" && containsStr(c.cmd[1], ".gc-workspace-ready") {
				t.Error("prebaked Start should not touch .gc-workspace-ready")
			}
			// Should not see gc init
			if len(c.cmd) >= 2 && c.cmd[0] == "gc" && c.cmd[1] == "init" {
				t.Error("prebaked Start should not run gc init")
			}
		}
	}
}

func TestStartDetectsImmediateSessionDeath(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0 // no delay in tests

	// tmux has-session succeeds during waitForTmux, then fails on post-start check.
	hasSessionCalls := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			hasSessionCalls++
			if hasSessionCalls <= 1 {
				return "", nil // first call: tmux alive (waitForTmux)
			}
			return "", fmt.Errorf("no server running on /tmp/tmux-1000/default")
		}
		return "", nil
	}

	cfg := runtime.Config{
		Command:      "claude --resume stale-key",
		Env:          map[string]string{"GC_AGENT": "deacon", "GC_CITY": "/workspace"},
		ProcessNames: []string{"claude"},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err == nil {
		t.Fatal("Start should fail when session dies immediately after startup")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Start error = %v, want ErrSessionDiedDuringStartup", err)
	}

	// Pod should have been cleaned up.
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Error("pod should have been deleted after immediate session death")
	}
}

func TestStartAllowsOneShotLifecycleCommands(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{
			name:    "direct agent script",
			command: "gc agent-script --script /workspace/rig/assets/scripts/hyperscale-worker.yaml",
		},
		{
			name:    "wrapped one shot",
			command: "env GC_LOG_LEVEL=debug custom-once --work",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeK8sOps()
			p := newProviderWithOps(fake)
			p.postStartSettle = 100 * time.Millisecond

			hasSessionCalls := 0
			fake.execFunc = func(_ string, cmd []string) (string, error) {
				if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
					hasSessionCalls++
					if hasSessionCalls == 1 {
						return "", nil
					}
					return "", fmt.Errorf("no server running on /tmp/tmux-1000/default")
				}
				return "", nil
			}

			cfg := runtime.Config{
				Command:   tt.command,
				Env:       map[string]string{"GC_AGENT": "hyperscale/worker", "GC_CITY": "/workspace"},
				Lifecycle: runtime.LifecycleOneShot,
				Nudge:     "Check your hook for work.",
			}

			started := time.Now()
			err := p.Start(context.Background(), "gc-test-agent", cfg)
			if err != nil {
				t.Fatalf("Start should allow one-shot lifecycle command: %v", err)
			}
			if elapsed := time.Since(started); elapsed >= p.postStartSettle {
				t.Fatalf("Start returned after %v, want before settle duration %v", elapsed, p.postStartSettle)
			}
			if hasSessionCalls != 1 {
				t.Fatalf("tmux has-session calls = %d, want only waitForTmux check", hasSessionCalls)
			}
			if _, exists := fake.pods["gc-test-agent"]; !exists {
				t.Fatal("pod should remain for normal session reconciliation after one-shot command")
			}
		})
	}
}

func TestStartChecksLivenessForScriptCommandWithoutOneShotLifecycle(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	hasSessionCalls := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			hasSessionCalls++
			if hasSessionCalls == 1 {
				return "", nil
			}
			return "", fmt.Errorf("no server running on /tmp/tmux-1000/default")
		}
		return "", nil
	}

	cfg := runtime.Config{
		Command: "gc agent-script --script /workspace/rig/assets/scripts/hyperscale-worker.yaml",
		Env:     map[string]string{"GC_AGENT": "hyperscale/worker", "GC_CITY": "/workspace"},
		Nudge:   "Check your hook for work.",
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Start error = %v, want ErrSessionDiedDuringStartup", err)
	}
	if hasSessionCalls != 2 {
		t.Fatalf("tmux has-session calls = %d, want waitForTmux and post-start liveness checks", hasSessionCalls)
	}
}

func TestStartChecksLivenessForCustomCommandWithSetupAndNudgeHints(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	// tmux has-session succeeds during waitForTmux, then fails on post-start check.
	hasSessionCalls := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			hasSessionCalls++
			if hasSessionCalls == 1 {
				return "", nil
			}
			return "", fmt.Errorf("no server running on /tmp/tmux-1000/default")
		}
		return "", nil
	}

	cfg := runtime.Config{
		Command:      "custom-agent --interactive",
		Env:          map[string]string{"GC_AGENT": "custom/worker", "GC_CITY": "/workspace"},
		SessionSetup: []string{"printf setup-ready >/tmp/agent-ready"},
		Nudge:        "Check your hook for work.",
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Start error = %v, want ErrSessionDiedDuringStartup", err)
	}
	if hasSessionCalls != 2 {
		t.Fatalf("tmux has-session calls = %d, want waitForTmux and post-start liveness checks", hasSessionCalls)
	}
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Error("pod should have been deleted after immediate session death")
	}
}

func TestStartSucceedsWhenSessionStaysAlive(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	// tmux has-session always succeeds.
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command:      "claude --session-id fresh-key",
		Env:          map[string]string{"GC_AGENT": "deacon", "GC_CITY": "/workspace"},
		ProcessNames: []string{"claude"},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start should succeed when session stays alive: %v", err)
	}
}

func TestStartHonorsCancellationDuringPostStartSettle(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 100 * time.Millisecond

	hasSessionCalls := 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "tmux" && cmd[1] == "has-session" {
			hasSessionCalls++
		}
		return "", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	cfg := runtime.Config{
		Command:      "claude --session-id fresh-key",
		Env:          map[string]string{"GC_AGENT": "deacon", "GC_CITY": "/workspace"},
		ProcessNames: []string{"claude"},
	}

	started := time.Now()
	err := p.Start(ctx, "gc-test-agent", cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed >= p.postStartSettle {
		t.Fatalf("Start returned after %v, want before settle duration %v", elapsed, p.postStartSettle)
	}
	if hasSessionCalls != 1 {
		t.Fatalf("tmux has-session calls = %d, want 1 before settle cancellation", hasSessionCalls)
	}
	if _, exists := fake.pods["gc-test-agent"]; exists {
		t.Error("pod should have been deleted after settle cancellation")
	}
}

func TestStartSendsNudge(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0
	p.runningPodCacheTTL = time.Hour
	if names, err := p.ListRunning(""); err != nil || len(names) != 0 {
		t.Fatalf("prime empty running-pod snapshot: names=%v err=%v", names, err)
	}

	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		Env: map[string]string{
			"GC_AGENT": "deacon",
			"GC_CITY":  "/workspace",
		},
		Nudge: "Run 'gc prime' to check patrol status.",
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify nudge was sent via tmux send-keys.
	var foundText, foundEnter bool
	for _, c := range fake.calls {
		if c.method != "execInPod" {
			continue
		}
		if len(c.cmd) >= 6 && c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" && c.cmd[4] == "-l" {
			foundText = true
			if c.cmd[5] != cfg.Nudge {
				t.Errorf("nudge text = %q, want %q", c.cmd[5], cfg.Nudge)
			}
		}
		if len(c.cmd) == 5 && c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" && c.cmd[4] == "Enter" {
			foundEnter = true
		}
	}
	if !foundText {
		t.Error("Start did not send nudge text via tmux send-keys")
	}
	if !foundEnter {
		t.Error("Start did not send Enter after nudge text")
	}
}

func TestStartSkipsNudgeWhenEmpty(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command: "claude --settings .gc/settings.json",
		Env: map[string]string{
			"GC_AGENT": "mayor",
			"GC_CITY":  "/workspace",
		},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify no send-keys calls with -l flag (nudge text).
	for _, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) >= 5 &&
			c.cmd[0] == "tmux" && c.cmd[1] == "send-keys" && c.cmd[4] == "-l" {
			t.Error("Start sent nudge text when Nudge was empty")
		}
	}
}

// --- Relaunch (un-weld B3a) ---

// findExecCmd returns the cmd of the first execInPod call whose joined cmd
// contains substr (nil if none).
func findExecCmd(fake *fakeK8sOps, substr string) []string { //nolint:unparam // substr varies in future tests
	for _, c := range fake.calls {
		if c.method == "execInPod" && strings.Contains(strings.Join(c.cmd, " "), substr) {
			return c.cmd
		}
	}
	return nil
}

func TestProvider_RelaunchRespawnsAgentInWarmPod(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	hasSessionAlive(fake, "s") // guard + liveness recheck both succeed

	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent --resume"}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}

	respawn := findExecCmd(fake, "respawn-pane")
	if respawn == nil {
		t.Fatal("Relaunch did not issue tmux respawn-pane over execInPod")
	}
	body := respawn[len(respawn)-1] // sh -c <body>
	if !strings.Contains(body, "tmux respawn-pane -k -t main") {
		t.Errorf("respawn body = %q, want it to respawn the 'main' session in place", body)
	}
	// The command is base64-shipped, not inlined verbatim.
	wantB64 := base64.StdEncoding.EncodeToString([]byte("agent --resume"))
	if !strings.Contains(body, wantB64) {
		t.Errorf("respawn body = %q, want base64 %q of the agent command", body, wantB64)
	}
	if strings.Contains(body, "agent --resume") {
		t.Errorf("respawn body = %q leaked the raw command; it must be base64-shipped", body)
	}
	// Warm reuse: no pod was created or deleted.
	for _, c := range fake.calls {
		if c.method == "createPod" || c.method == "deletePod" {
			t.Errorf("Relaunch must reuse the warm pod, but called %s", c.method)
		}
	}
}

func TestProvider_RelaunchMissingPodIsErrSessionNotFound(t *testing.T) {
	fake := newFakeK8sOps() // no pods
	p := newProviderWithOps(fake)
	err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent"})
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Relaunch err = %v, want ErrSessionNotFound", err)
	}
	if findExecCmd(fake, "respawn-pane") != nil {
		t.Error("respawn-pane must not be issued when there is no running pod")
	}
}

func TestProvider_RelaunchDeadTmuxIsErrSessionNotFound(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	// Pod runs but tmux "main" is gone: relaunch must NOT recreate the pod.
	fake.setExecResult("s", []string{"tmux", "has-session", "-t", tmuxSession}, "", errors.New("no session"))
	err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent"})
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Relaunch err = %v, want ErrSessionNotFound", err)
	}
	if findExecCmd(fake, "respawn-pane") != nil {
		t.Error("respawn-pane must not be issued when the tmux session is dead")
	}
}

func TestProvider_RelaunchSuWrapsForLinuxUsername(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	hasSessionAlive(fake, "s")

	cfg := runtime.Config{Command: "agent", Env: map[string]string{"LINUX_USERNAME": "dev"}}
	if err := p.Relaunch(context.Background(), "s", cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	body := findExecCmd(fake, "respawn-pane")
	if body == nil {
		t.Fatal("no respawn-pane call")
	}
	last := body[len(body)-1]
	if !strings.Contains(last, `su - dev -c`) {
		t.Errorf("respawn body = %q, want it su-wrapped for the LINUX_USERNAME tmux socket", last)
	}
}

// --- Test helpers ---

func addRunningPod(fake *fakeK8sOps, name, sessionLabel string) { //nolint:unparam // name varies in future tests
	fake.pods[name] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			UID:    types.UID("test-uid-" + name),
			Labels: map[string]string{"app": "gc-agent", "gc-session": sessionLabel},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// addFailedPod adds a pod that exists by session label but is NOT Running, so
// IsRunning(name) is false while Stop (list-by-label, any phase) still finds it.
func addFailedPod(fake *fakeK8sOps, name, sessionLabel string) { //nolint:unparam // name varies in future tests
	fake.pods[name] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			UID:    types.UID("test-uid-" + name),
			Labels: map[string]string{"app": "gc-agent", "gc-session": sessionLabel},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
}

func addRunningPodWithAnnotation(fake *fakeK8sOps, name, sessionLabel, sessionName string) {
	fake.pods[name] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			UID:         types.UID("test-uid-" + name),
			Labels:      map[string]string{"app": "gc-agent", "gc-session": sessionLabel},
			Annotations: map[string]string{"gc-session-name": sessionName},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestBuildPodServiceAccount(t *testing.T) {
	cfg := runtime.Config{
		Command: "/bin/bash",
		Env:     map[string]string{"GC_AGENT": "test"},
	}

	t.Run("sets ServiceAccountName when configured", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())
		p.serviceAccount = "gc-agent"

		pod, err := buildPod("test-pod", cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		if pod.Spec.ServiceAccountName != "gc-agent" {
			t.Errorf("ServiceAccountName = %q, want %q", pod.Spec.ServiceAccountName, "gc-agent")
		}
	})

	t.Run("leaves ServiceAccountName empty when not configured", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())

		pod, err := buildPod("test-pod", cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		if pod.Spec.ServiceAccountName != "" {
			t.Errorf("ServiceAccountName = %q, want empty", pod.Spec.ServiceAccountName)
		}
	})
}

func TestInitCityInPodMaterializesPacksAndImports(t *testing.T) {
	fake := newFakeK8sOps()

	err := initCityInPod(context.Background(), fake, "gc-mayor", "/city")
	if err != nil {
		t.Fatalf("initCityInPod: %v", err)
	}

	// gc init must run with GC_DOLT=skip so it does not attempt to start a
	// local Dolt server. In K8s pods, the in-cluster Dolt service is set up
	// separately by verifyBeadsInPod.
	var gcInitCmd, packCopyCmd, importInstallCmd []string
	initIndex, packIndex, importIndex, cleanupIndex := -1, -1, -1, -1
	for index, c := range fake.calls {
		if c.method == "execInPod" && len(c.cmd) > 0 {
			joined := strings.Join(c.cmd, " ")
			switch {
			case strings.Contains(joined, "gc init") &&
				strings.Contains(joined, "--from /tmp/city-src /workspace"):
				gcInitCmd = c.cmd
				initIndex = index
			case strings.Contains(joined, "cp -a /tmp/city-src/packs/. /workspace/packs/"):
				packCopyCmd = c.cmd
				packIndex = index
			case joined == "gc --city /workspace import install":
				importInstallCmd = c.cmd
				importIndex = index
			case joined == "rm -rf /tmp/city-src":
				cleanupIndex = index
			}
		}
	}
	if gcInitCmd == nil {
		t.Fatal("gc init command not found in exec calls")
	}

	hasDoltSkip := false
	hasProviderReadinessSkip := false
	hasNoStart := false
	for _, arg := range gcInitCmd {
		switch arg {
		case "GC_DOLT=skip":
			hasDoltSkip = true
		case "--skip-provider-readiness":
			hasProviderReadinessSkip = true
		case "--no-start":
			hasNoStart = true
		}
	}
	if !hasDoltSkip {
		t.Errorf("gc init should run with GC_DOLT=skip; got cmd=%v", gcInitCmd)
	}
	if !hasProviderReadinessSkip {
		t.Errorf("gc init should skip provider readiness in an isolated worker pod; got cmd=%v", gcInitCmd)
	}
	if !hasNoStart {
		t.Errorf("gc init should not start a nested supervisor in an isolated worker pod; got cmd=%v", gcInitCmd)
	}
	if packCopyCmd == nil {
		t.Fatal("local city packs were not materialized before gc init")
	}
	if importInstallCmd == nil {
		t.Fatal("locked city imports were not installed")
	}
	// gc init resolves every rig's local imports while it loads city.toml.
	// The authored packs must therefore exist in /workspace before init runs.
	if packIndex >= initIndex || initIndex >= importIndex || importIndex >= cleanupIndex {
		t.Fatalf(
			"city initialization order = packs:%d init:%d imports:%d cleanup:%d",
			packIndex, initIndex, importIndex, cleanupIndex,
		)
	}
}

func TestInitCityInPodImportFailureCleansTemporaryCity(t *testing.T) {
	fake := newFakeK8sOps()
	importCmd := []string{"gc", "--city", "/workspace", "import", "install"}
	fake.setExecResult("gc-mayor", importCmd, "", errors.New("locked import unavailable"))

	err := initCityInPod(context.Background(), fake, "gc-mayor", "/city")
	if err == nil || !strings.Contains(err.Error(), "installing locked city imports") {
		t.Fatalf("initCityInPod error = %v, want locked import failure", err)
	}

	cleanupCmd := findExecCmd(fake, "rm -rf /tmp/city-src")
	if cleanupCmd == nil {
		t.Fatal("temporary city source was not cleaned after import failure")
	}
}

// deadlineCheckingListExecOps wraps fakeK8sOps and fails listPods/execInPod
// unless called with a tightly bounded context deadline. It extends the
// pattern PR #54 (gcy-bru) used for the GC_INSTANCE_TOKEN probe to the
// sibling call sites gcy-qla found still issuing unbounded LIST/EXEC calls on
// a liveness/reconcile cadence: ProcessAlive, SetMeta, GetMeta's non-token
// fallback, and GetLastActivity. A wedged or partitioned k8s API server must
// surface as a bounded error, not hang whatever loop calls these.
type deadlineCheckingListExecOps struct {
	*fakeK8sOps
}

func (o *deadlineCheckingListExecOps) listPods(ctx context.Context, selector, fieldSelector string) ([]corev1.Pod, error) {
	if err := requireTightDeadline(ctx); err != nil {
		return nil, err
	}
	return o.fakeK8sOps.listPods(ctx, selector, fieldSelector)
}

func (o *deadlineCheckingListExecOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	if err := requireTightDeadline(ctx); err != nil {
		return "", err
	}
	return o.fakeK8sOps.execInPod(ctx, pod, container, cmd, stdin)
}

func requireTightDeadline(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("k8s call has no context deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 10*time.Second {
		return fmt.Errorf("k8s call deadline is not tightly bounded: %s", remaining)
	}
	return nil
}

// TestProcessAliveBoundsCallsWithDeadline is the regression guard for
// gcy-qla: ProcessAlive is reachable from liveness probes and doctor checks
// on a recurring cadence, so it fails against the pre-fix context.Background()
// call and passes once it reuses runningPodSnapshotTimeout.
func TestProcessAliveBoundsCallsWithDeadline(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent", []string{"pgrep", "-f", "claude"}, "1234\n", nil)
	p := newProviderWithOps(&deadlineCheckingListExecOps{fakeK8sOps: fake})

	if !p.ProcessAlive("gc-test-agent", []string{"claude"}) {
		t.Fatal("ProcessAlive with a tightly bounded context available = false, want true")
	}
}

// TestSetMetaBoundsCallsWithDeadline is the regression guard for gcy-qla:
// SetMeta's findPod/execInPod calls must never be unbounded.
func TestSetMetaBoundsCallsWithDeadline(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	p := newProviderWithOps(&deadlineCheckingListExecOps{fakeK8sOps: fake})

	if err := p.SetMeta("gc-test-agent", "GC_DRAIN", "true"); err != nil {
		t.Fatalf("SetMeta with a tightly bounded context available: %v", err)
	}
}

// TestGetMetaNonTokenFallbackBoundsCallsWithDeadline is the regression guard
// for gcy-qla: GetMeta's non-token fallback (the findPod/execInPod path used
// for every key other than GC_INSTANCE_TOKEN) must be bounded the same way
// the GC_INSTANCE_TOKEN branch already is (gcy-bru/PR #54).
func TestGetMetaNonTokenFallbackBoundsCallsWithDeadline(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "show-environment", "-t", tmuxSession, "GC_DRAIN"}, "GC_DRAIN=true\n", nil)
	p := newProviderWithOps(&deadlineCheckingListExecOps{fakeK8sOps: fake})

	got, err := p.GetMeta("gc-test-agent", "GC_DRAIN")
	if err != nil {
		t.Fatalf("GetMeta with a tightly bounded context available: %v", err)
	}
	if got != "true" {
		t.Fatalf("GetMeta(GC_DRAIN) = %q, want %q", got, "true")
	}
}

// TestGetLastActivityBoundsCallsWithDeadline is the regression guard for
// gcy-qla: GetLastActivity is reachable from the session reconciler and
// manager on a reconcile cadence, so its calls must never be unbounded.
func TestGetLastActivityBoundsCallsWithDeadline(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "gc-test-agent", "gc-test-agent")
	fake.setExecResult("gc-test-agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_activity}"},
		"1709300000\n", nil)
	p := newProviderWithOps(&deadlineCheckingListExecOps{fakeK8sOps: fake})

	activity, err := p.GetLastActivity("gc-test-agent")
	if err != nil {
		t.Fatalf("GetLastActivity with a tightly bounded context available: %v", err)
	}
	if want := time.Unix(1709300000, 0); !activity.Equal(want) {
		t.Fatalf("GetLastActivity = %v, want %v", activity, want)
	}
}
