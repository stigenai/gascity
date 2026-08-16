package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

const runningPodSnapshotTimeout = 5 * time.Second

// Compile-time interface checks.
var (
	_ runtime.Provider                        = (*Provider)(nil)
	_ runtime.ExecProvider                    = (*Provider)(nil)
	_ runtime.FreshRunningSessionLister       = (*Provider)(nil)
	_ runtime.InstanceTokenFencedStopProvider = (*Provider)(nil)
	_ runtime.ProcessAliveChecker             = (*Provider)(nil)
	_ runtime.RunningChecker                  = (*Provider)(nil)
	_ runtime.AttachedChecker                 = (*Provider)(nil)
	_ runtime.CapsuleStateRuntime             = (*Provider)(nil)
	_ runtime.CapsuleStatePlace               = (*Provider)(nil)
)

// Provider is a native Kubernetes session provider using client-go.
// Eliminates subprocess overhead by making direct API calls over reused
// HTTP/2 connections. Pod manifests are compatible with gc-session-k8s.
type Provider struct {
	ops                     k8sOps
	pvcOps                  k8sPVCOps
	networkOps              k8sNetworkOps
	namespace               string
	image                   string
	k8sContext              string
	managedServiceHost      string
	managedServicePort      string
	cpuRequest              string
	memRequest              string
	cpuLimit                string
	memLimit                string
	serviceAccount          string              // pod service account name (GC_K8S_SERVICE_ACCOUNT)
	prebaked                bool                // skip staging + init container for prebaked images
	nodeSelector            map[string]string   // GC_K8S_NODE_SELECTOR (JSON)
	tolerations             []corev1.Toleration // GC_K8S_TOLERATIONS (JSON)
	affinity                *corev1.Affinity    // GC_K8S_AFFINITY (JSON)
	priorityClassName       string              // GC_K8S_PRIORITY_CLASS_NAME
	capsuleCityScope        string              // GC_K8S_CAPSULE_CITY_SCOPE
	capsuleStorageRequest   string              // GC_K8S_CAPSULE_STORAGE_REQUEST
	capsuleStorageClassName string              // GC_K8S_CAPSULE_STORAGE_CLASS
	postStartSettle         time.Duration       // settle time before post-start liveness check
	stderr                  io.Writer           // warning output (default os.Stderr)
	attachStdin             io.Reader
	attachStdout            io.Writer
	attachStderr            io.Writer
	runAttachCommand        attachCommandRunner
	runningPodCacheMu       sync.RWMutex
	runningPodCache         *runningPodState
	runningPodCacheAt       time.Time
	runningPodCacheTTL      time.Duration
	runningPodCacheGen      uint64
	runningPodFlight        singleflight.Group
}

type runningPodState struct {
	bySession         map[string]string
	agentSessionNames []string
}

type schedulingFields struct {
	nodeSelector      map[string]string
	tolerations       []corev1.Toleration
	affinity          *corev1.Affinity
	priorityClassName string
}

// NewProvider creates a K8s session provider.
// Configuration is read from environment variables (matching gc-session-k8s):
//   - GC_K8S_NAMESPACE — namespace (default: "gc")
//   - GC_K8S_IMAGE — container image (required for Start)
//   - GC_K8S_CONTEXT — kubectl context (default: current)
//   - GC_K8S_SERVICE_ACCOUNT — pod service account name (default: namespace default)
//   - GC_K8S_CPU_REQUEST, GC_K8S_MEM_REQUEST — resource requests
//   - GC_K8S_CPU_LIMIT, GC_K8S_MEM_LIMIT — resource limits
//   - GC_K8S_DOLT_SECRET — namespace-local Secret containing username/password
//   - GC_K8S_CAPSULE_CITY_SCOPE — trusted stable identity for capsule PVC ownership
//   - GC_K8S_CAPSULE_STORAGE_REQUEST — per-session capsule PVC request (default: 10Gi)
//   - GC_K8S_CAPSULE_STORAGE_CLASS — optional StorageClass for capsule PVCs
//
// The in-cluster Dolt service alias defaults to the provider defaults
// (dolt.gc.svc.cluster.local:3307). Pods receive projected GC_DOLT_* env;
// GC_K8S_DOLT_* remains a deprecated compatibility input for the provider-
// managed in-cluster alias only. When GC_K8S_DOLT_SECRET is set, both the GC
// and BEADS credential variables are required SecretKeyRef projections; Dolt
// credential literals are never copied from the controller into Pod specs.
//
// Uses rest.InClusterConfig() when running in a pod, falls back to
// clientcmd.BuildConfigFromFlags() for local development.
func NewProvider() (*Provider, error) {
	namespace := envOrDefault("GC_K8S_NAMESPACE", "gc")
	image := os.Getenv("GC_K8S_IMAGE")
	k8sContext := os.Getenv("GC_K8S_CONTEXT")

	restConfig, err := buildRESTConfig(k8sContext)
	if err != nil {
		return nil, fmt.Errorf("building K8s config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating K8s clientset: %w", err)
	}

	managedServiceHost, managedServicePort, err := managedServiceAlias()
	if err != nil {
		return nil, err
	}

	scheduling, err := parseSchedulingEnv()
	if err != nil {
		return nil, err
	}

	realOps := &realK8sOps{clientset: clientset, restConfig: restConfig, namespace: namespace}
	return &Provider{
		ops:                     realOps,
		pvcOps:                  realOps,
		networkOps:              realOps,
		namespace:               namespace,
		image:                   image,
		k8sContext:              k8sContext,
		managedServiceHost:      managedServiceHost,
		managedServicePort:      managedServicePort,
		cpuRequest:              envOrDefault("GC_K8S_CPU_REQUEST", "500m"),
		memRequest:              envOrDefault("GC_K8S_MEM_REQUEST", "1Gi"),
		cpuLimit:                envOrDefault("GC_K8S_CPU_LIMIT", "2"),
		memLimit:                envOrDefault("GC_K8S_MEM_LIMIT", "4Gi"),
		serviceAccount:          os.Getenv("GC_K8S_SERVICE_ACCOUNT"),
		prebaked:                os.Getenv("GC_K8S_PREBAKED") == "true",
		postStartSettle:         3 * time.Second,
		stderr:                  os.Stderr,
		attachStdin:             os.Stdin,
		attachStdout:            os.Stdout,
		attachStderr:            os.Stderr,
		nodeSelector:            scheduling.nodeSelector,
		tolerations:             scheduling.tolerations,
		affinity:                scheduling.affinity,
		priorityClassName:       scheduling.priorityClassName,
		capsuleCityScope:        strings.TrimSpace(os.Getenv("GC_K8S_CAPSULE_CITY_SCOPE")),
		capsuleStorageRequest:   envOrDefault("GC_K8S_CAPSULE_STORAGE_REQUEST", "10Gi"),
		capsuleStorageClassName: strings.TrimSpace(os.Getenv("GC_K8S_CAPSULE_STORAGE_CLASS")),
		// Long enough to cover one bounded concurrent EnrichInfos page and the
		// session-stream precheck that follows it; short enough that external pod
		// state changes remain visible on the next controller tick.
		runningPodCacheTTL: 2 * time.Second,
	}, nil
}

func parseSchedulingEnv() (schedulingFields, error) {
	var scheduling schedulingFields
	if v := os.Getenv("GC_K8S_NODE_SELECTOR"); v != "" {
		if err := json.Unmarshal([]byte(v), &scheduling.nodeSelector); err != nil {
			return schedulingFields{}, fmt.Errorf("parsing GC_K8S_NODE_SELECTOR: %w", err)
		}
	}
	if v := os.Getenv("GC_K8S_TOLERATIONS"); v != "" {
		if err := json.Unmarshal([]byte(v), &scheduling.tolerations); err != nil {
			return schedulingFields{}, fmt.Errorf("parsing GC_K8S_TOLERATIONS: %w", err)
		}
	}
	if v := os.Getenv("GC_K8S_AFFINITY"); v != "" {
		if err := json.Unmarshal([]byte(v), &scheduling.affinity); err != nil {
			return schedulingFields{}, fmt.Errorf("parsing GC_K8S_AFFINITY: %w", err)
		}
	}
	scheduling.priorityClassName = os.Getenv("GC_K8S_PRIORITY_CLASS_NAME")
	return scheduling, nil
}

// newProviderWithOps creates a provider with a custom k8sOps (for testing).
func newProviderWithOps(ops k8sOps) *Provider {
	pvcOps, _ := ops.(k8sPVCOps)
	networkOps, _ := ops.(k8sNetworkOps)
	return &Provider{
		ops:                   ops,
		pvcOps:                pvcOps,
		networkOps:            networkOps,
		namespace:             "test-ns",
		image:                 "test-image:latest",
		managedServiceHost:    podManagedDoltHost,
		managedServicePort:    podManagedDoltPort,
		cpuRequest:            "500m",
		memRequest:            "1Gi",
		cpuLimit:              "2",
		memLimit:              "4Gi",
		capsuleCityScope:      "cluster/test-ns/city",
		capsuleStorageRequest: "10Gi",
		stderr:                io.Discard,
		attachStdin:           strings.NewReader(""),
		attachStdout:          io.Discard,
		attachStderr:          io.Discard,
	}
}

// Start creates a new K8s pod running a tmux session with the agent command.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.image == "" {
		return fmt.Errorf("starting session %q: GC_K8S_IMAGE is required", name)
	}
	if err := p.validateCapsuleStateForStart(ctx, cfg); err != nil {
		return fmt.Errorf("validating capsule state for session %q: %w", name, err)
	}
	if cfg.Capsule != nil {
		if err := p.AttachCapsuleState(ctx, name, cfg.Capsule.State); err != nil {
			return fmt.Errorf("attaching capsule state for session %q: %w", name, err)
		}
	}
	if err := p.preflightCapsuleNetwork(ctx, cfg); err != nil {
		return fmt.Errorf("validating capsule network for session %q: %w", name, err)
	}
	podName := SanitizeName(name)
	label := SanitizeLabel(name)

	// Check for existing pod (any phase).
	existing, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		return fmt.Errorf("listing existing pods for session %q: %w", name, err)
	}
	if cfg.Capsule != nil {
		for i := range existing {
			if existing[i].Name == podName && (existing[i].Labels["gc-capsule"] != "true" || !p.podBelongsToCapsuleCity(&existing[i])) {
				return fmt.Errorf("%w: pod %q for session %q belongs to another capsule city", runtime.ErrCapsuleStateConflict, podName, name)
			}
		}
		existing = p.capsulePodsForCity(existing)
	}
	if len(existing) > 0 {
		pod := &existing[0]
		if pod.Status.Phase == corev1.PodRunning {
			// Check if tmux is alive — stale pod detection.
			_, tmuxErr := p.ops.execInPod(ctx, pod.Name, "agent",
				[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
			if tmuxErr == nil {
				return fmt.Errorf("%w: session %q (pod: %s)", runtime.ErrSessionExists, name, pod.Name)
			}
			// tmux dead — but if the pod is young, workspace init may still
			// be blocking the tmux server from starting. Don't delete pods
			// that are still within the startup window.
			if time.Since(pod.CreationTimestamp.Time) < startupGracePeriod {
				return fmt.Errorf("%w: session %q (pod: %s)", runtime.ErrSessionInitializing, name, pod.Name)
			}
			// Stale pod — tmux dead and past grace period, recreate.
		}
		// Clean up existing pod.
		p.invalidateRunningPodSnapshot()
		delErr := p.ops.deletePod(ctx, pod.Name, pod.UID, 5)
		p.invalidateRunningPodSnapshot()
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("deleting existing pod %q for session %q: %w", pod.Name, name, delErr)
		}
		if waitErr := waitForDeletion(ctx, p.ops, pod.Name, 30*time.Second); waitErr != nil {
			return fmt.Errorf("waiting for existing pod %q deletion: %w", pod.Name, waitErr)
		}
	}
	// Build and create pod.
	pod, err := buildPod(name, cfg, p)
	if err != nil {
		return fmt.Errorf("building pod for session %q: %w", name, err)
	}
	networkPolicy, networkPolicyCreated, err := p.ensureCapsuleNetworkPolicy(ctx, name, cfg)
	if err != nil {
		return fmt.Errorf("applying capsule network isolation for session %q: %w", name, err)
	}
	cleanupNetworkPolicy := func(reason string) error {
		if !networkPolicyCreated {
			return nil
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := p.networkOps.deleteNetworkPolicy(cleanupCtx, networkPolicy.Name, networkPolicy.UID)
		if err == nil || apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("cleaning up NetworkPolicy %q for session %q after %s: %w", networkPolicy.Name, name, reason, err)
	}
	p.invalidateRunningPodSnapshot()
	createdPod, err := p.ops.createPod(ctx, pod)
	p.invalidateRunningPodSnapshot()
	if err != nil {
		createErr := fmt.Errorf("creating pod for session %q: %w", name, err)
		if cfg.Capsule != nil {
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			current, getErr := p.ops.getPod(recoveryCtx, podName)
			cancel()
			if getErr == nil {
				if validateErr := validateCommittedCapsulePod(current, pod); validateErr != nil {
					return errors.Join(createErr, validateErr, cleanupNetworkPolicy("pod create identity conflict"))
				}
				createdPod = current
				err = nil
			}
		}
		if err != nil {
			return errors.Join(createErr, cleanupNetworkPolicy("pod creation failed"))
		}
	}
	if createdPod == nil || createdPod.UID == "" {
		return errors.Join(
			fmt.Errorf("creating pod for session %q returned no immutable UID", name),
			cleanupNetworkPolicy("pod create returned no UID"),
		)
	}
	podUID := createdPod.UID

	// cleanup deletes the pod on any startup failure after creation.
	// Uses a fresh background context so cleanup succeeds even if the
	// original ctx was canceled (which is the common failure path).
	cleanup := func(reason string) error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var cleanupErrs []error
		p.invalidateRunningPodSnapshot()
		if err := p.ops.deletePod(cleanupCtx, podName, podUID, 5); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("cleaning up pod %q for session %q after %s: %w", podName, name, reason, err))
		}
		p.invalidateRunningPodSnapshot()
		if networkPolicyCreated {
			if err := p.networkOps.deleteNetworkPolicy(cleanupCtx, networkPolicy.Name, networkPolicy.UID); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("cleaning up NetworkPolicy %q for session %q after %s: %w", networkPolicy.Name, name, reason, err))
			}
		}
		return errors.Join(cleanupErrs...)
	}

	ctrlCity := cfg.Env["GC_CITY"]

	if !p.prebaked {
		// Stage files via init container if needed.
		if needsStaging(cfg, ctrlCity) {
			if err := stageFiles(ctx, p.ops, podName, cfg, ctrlCity, p.stderr); err != nil {
				return errors.Join(fmt.Errorf("staging files for session %q: %w", name, err), cleanup("staging failed"))
			}
		}
	}

	// Wait for main container to be running.
	if err := waitForPodRunning(ctx, p.ops, podName, 120*time.Second); err != nil {
		return errors.Join(fmt.Errorf("waiting for pod %q: %w", podName, err), cleanup("pod not running"))
	}

	if !p.prebaked {
		// Initialize the city inside the pod.
		if ctrlCity != "" {
			if err := initCityInPod(ctx, p.ops, podName, ctrlCity); err != nil {
				return errors.Join(fmt.Errorf("initializing city for session %q: %w", name, err), cleanup("city initialization failed"))
			}
		}

		// Signal entrypoint to proceed.
		if _, err := p.ops.execInPod(ctx, podName, "agent",
			[]string{"touch", "/workspace/.gc-workspace-ready"}, nil); err != nil {
			fmt.Fprintf(p.stderr, "gc: warning: touch .gc-workspace-ready in %s: %v\n", podName, err) //nolint:errcheck
		}
	}

	// Ensure .beads/ inside the pod. This remains warning-only so older staged
	// or prebaked workspaces can self-heal instead of failing session startup.
	podWorkDir := projectedPodWorkDir(cfg)
	if err := initBeadsInPod(ctx, p.ops, podName, cfg, podWorkDir, p.managedServiceHost, p.managedServicePort); err != nil {
		fmt.Fprintf(p.stderr, "gc: warning: initBeadsInPod for %s: %v\n", podName, err) //nolint:errcheck
	}

	// Wait for tmux session.
	if err := waitForTmux(ctx, p.ops, podName, 60*time.Second); err != nil {
		return errors.Join(fmt.Errorf("waiting for tmux in pod %q: %w", podName, err), cleanup("tmux not ready"))
	}

	// Enable pane logging + run session setup (shared with the relaunch tail).
	p.runPodPostLaunchSetup(ctx, podName, cfg)

	requiresPostStartLiveness := k8sRequiresPostStartLiveness(cfg)

	// Post-start liveness check: verify interactive sessions survived startup.
	// Agents that fail immediately (e.g. --resume with a stale session key)
	// exit within a second. A brief settle lets us detect this before
	// returning success to the reconciler, which triggers recordWakeFailure
	// and the crash-loop recovery (clear session_key, bump continuation_epoch).
	//
	// Some configured commands are intentionally one-turn processes. Those
	// should return from Start after the first tmux appearance and let normal
	// session reconciliation observe completion, rather than converting clean
	// command exit into startup failure.
	if requiresPostStartLiveness && p.postStartSettle > 0 {
		timer := time.NewTimer(p.postStartSettle)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("waiting for post-start settle for session %q: %w", name, ctx.Err()), cleanup("post-start settle canceled"))
		case <-timer.C:
		}
	}
	if requiresPostStartLiveness {
		_, tmuxErr := p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
		if tmuxErr != nil {
			return errors.Join(
				fmt.Errorf("%w: session %q died immediately after startup: %w", runtime.ErrSessionDiedDuringStartup, name, tmuxErr),
				cleanup("session died immediately after startup"),
			)
		}
	}

	// A snapshot taken during the startup sequence cannot serve the initial
	// nudge or post-Start observation. Mutation attempts already establish their
	// pre-call boundaries; this terminal invalidation also drops any observation
	// populated while the pod was still initializing.
	p.invalidateRunningPodSnapshot()

	// Send initial nudge if configured (matches tmux adapter step 6).
	if cfg.Nudge != "" {
		_ = p.Nudge(name, runtime.TextContent(cfg.Nudge))
	}

	return nil
}

// runPodPostLaunchSetup enables pane logging and runs session_setup and
// session_setup_script inside the pod, best-effort. Shared by Start (after the
// entrypoint launches the agent) and Relaunch (after the respawn). k8s does not
// run SessionLive (RunLive is a no-op), matching the pre-un-weld behavior.
func (p *Provider) runPodPostLaunchSetup(ctx context.Context, podName string, cfg runtime.Config) {
	// Enable pane logging for diagnostics.
	_, _ = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "pipe-pane", "-t", tmuxSession, "-o", "cat >> /tmp/agent-output.log"}, nil)

	// Run session_setup commands inside the pod.
	for _, cmd := range cfg.SessionSetup {
		if cmd == "" {
			continue
		}
		_, _ = p.ops.execInPod(ctx, podName, "agent",
			[]string{"sh", "-c", cmd}, nil)
	}

	// Run session_setup_script.
	if cfg.SessionSetupScript != "" {
		script, err := os.ReadFile(cfg.SessionSetupScript)
		if err != nil {
			fmt.Fprintf(p.stderr, "gc: warning: reading session_setup_script %q for %s: %v\n", cfg.SessionSetupScript, podName, err) //nolint:errcheck
		} else {
			_, _ = p.ops.execInPod(ctx, podName, "agent",
				[]string{"sh"}, strings.NewReader(string(script)))
		}
	}
}

// Relaunch re-launches the agent inside the already-running (warm) pod without
// recreating it: it respawns the in-pod tmux "main" pane (respawn-pane -k) with
// the (possibly changed) command over execInPod, then re-runs the post-launch
// setup tail. The pod stays warm via the entrypoint's `sleep infinity`, so a
// launch-only config change reaches the live pod without a full reprovision —
// the k8s half of the runtime/transport un-weld (B3a, mirroring tmux B1 / ssh).
//
// The pod must be Running with a live tmux "main" session, else
// [runtime.ErrSessionNotFound] (the reconciler decides whether to Start fresh —
// it does NOT recreate the pod here). Staging, city/beads init, and PreStart are
// NOT re-run here (k8s treats PreStart as provision-half); env is provision-half
// too (set in the pod spec at create time, not re-injected — respawn-pane carries
// no env). NOTE: tmux diverges — as of the relaunch pre_start fix it re-runs
// PreStart on Relaunch (launch-half), while k8s and ssh intentionally do not.
//
// CAVEAT (unverified on a real cluster — see the B3 design doc): for
// LINUX_USERNAME pods the entrypoint runs tmux under `su - <user>`, so the
// respawn is su-wrapped to reach that user's tmux socket; and if the in-pod tmux
// server itself died (not just the agent), respawn-pane fails and Relaunch
// returns ErrSessionNotFound so the reconciler reprovisions.
func (p *Provider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: session %q (no running pod to relaunch into)", runtime.ErrSessionNotFound, name)
	}
	// The tmux server + "main" session must be alive to respawn into; a dead
	// session means the box is not warm enough — reprovision, don't respawn.
	if _, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "has-session", "-t", tmuxSession}, nil); err != nil {
		return fmt.Errorf("%w: session %q (pod %s has no live tmux session)", runtime.ErrSessionNotFound, name, podName)
	}

	// Respawn the agent in the warm "main" session.
	if _, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"sh", "-c", buildRespawnCommand(cfg)}, nil); err != nil {
		return fmt.Errorf("k8s relaunch %q: respawn-pane: %w", name, err)
	}

	// Re-run the post-launch setup tail (pipe-pane logging + session_setup[/script]).
	p.runPodPostLaunchSetup(ctx, podName, cfg)

	// Post-relaunch liveness: detect an agent that dies immediately.
	if k8sRequiresPostStartLiveness(cfg) {
		if p.postStartSettle > 0 {
			timer := time.NewTimer(p.postStartSettle)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return fmt.Errorf("k8s relaunch %q: %w", name, ctx.Err())
			case <-timer.C:
			}
		}
		if _, err := p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil); err != nil {
			return fmt.Errorf("%w: session %q died immediately after relaunch: %w",
				runtime.ErrSessionDiedDuringStartup, name, err)
		}
	}

	if cfg.Nudge != "" {
		_ = p.Nudge(name, runtime.TextContent(cfg.Nudge))
	}
	return nil
}

func k8sRequiresPostStartLiveness(cfg runtime.Config) bool {
	if cfg.Lifecycle == runtime.LifecycleOneShot {
		return false
	}
	return runtime.HasManagedStartupHints(cfg)
}

// Stop deletes the pod(s) for the named session. Missing/not-found is
// idempotent (no error: the session is genuinely gone), but a transport
// failure surfaces. A list failure is "unknown state", NOT "session gone" —
// swallowing it would let the seam adapter (Runtime.Teardown → Stop) drop
// tracking while pods and their PVCs keep running untracked, leaking the most
// expensive runtime. Delete errors are joined for the same reason; only a
// genuine Kubernetes NotFound (the pod raced to gone) is treated as idempotent.
// Every delete carries the immutable UID returned by the list, so a same-name
// replacement wins a precondition conflict instead of being cross-deleted.
// This mirrors the ssh provider's Stop discrimination.
func (p *Provider) Stop(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	label := SanitizeLabel(name)

	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // session genuinely gone
		}
		return fmt.Errorf("k8s stop %q: listing pods: %w", name, err)
	}
	// The targeted list is authoritative even when it is empty. Establish the
	// generation boundary before mutation, not after return: a delete may commit
	// and still report an error, and concurrent readers must not join the old
	// singleflight while that call is blocked.
	p.invalidateRunningPodSnapshot()
	var errs []error
	for i := range pods {
		if !p.podBelongsToCapsuleCity(&pods[i]) {
			continue
		}
		p.invalidateRunningPodSnapshot()
		delErr := p.ops.deletePod(ctx, pods[i].Name, pods[i].UID, 5)
		p.invalidateRunningPodSnapshot()
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			errs = append(errs, fmt.Errorf("deleting pod %q: %w", pods[i].Name, delErr))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("k8s stop %q: %w", name, errors.Join(errs...))
	}
	if err := p.deleteCapsuleNetworkPolicies(ctx, label, ""); err != nil {
		return fmt.Errorf("k8s stop %q: %w", name, err)
	}
	return nil
}

// StopIfInstanceToken destroys only pod objects whose immutable pod-spec token
// exactly equals expectedToken. The token comparison and delete targets come
// from one authoritative LIST; every delete carries the captured UID as a
// Kubernetes precondition, so a same-name replacement created after the LIST
// cannot be removed by this operation.
func (p *Provider) StopIfInstanceToken(name, expectedToken string) error {
	expectedToken = strings.TrimSpace(expectedToken)
	if expectedToken == "" {
		return fmt.Errorf("k8s fenced stop %q: %w: empty expected token", name, runtime.ErrRuntimeUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	label := SanitizeLabel(name)
	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		return fmt.Errorf("k8s fenced stop %q: %w: listing pods: %w", name, runtime.ErrRuntimeUnavailable, err)
	}
	if len(pods) == 0 {
		return fmt.Errorf("k8s fenced stop %q: %w", name, runtime.ErrSessionNotFound)
	}

	matched := make([]corev1.Pod, 0, len(pods))
	definiteMismatch := false
	identityUncertain := false
	for i := range pods {
		if !p.podBelongsToCapsuleCity(&pods[i]) {
			continue
		}
		token, ok := immutablePodInstanceToken(&pods[i])
		if !ok {
			identityUncertain = true
			continue
		}
		if token != expectedToken {
			definiteMismatch = true
			continue
		}
		matched = append(matched, pods[i])
	}
	if len(matched) == 0 {
		if identityUncertain {
			return fmt.Errorf("k8s fenced stop %q: %w: immutable instance token unavailable or ambiguous", name, runtime.ErrRuntimeUnavailable)
		}
		if definiteMismatch {
			return fmt.Errorf("k8s fenced stop %q: %w", name, runtime.ErrInstanceTokenMismatch)
		}
		return fmt.Errorf("k8s fenced stop %q: %w: no verifiable pod identity", name, runtime.ErrRuntimeUnavailable)
	}

	var errs []error
	for i := range matched {
		pod := &matched[i]
		p.invalidateRunningPodSnapshot()
		deleteErr := p.ops.deletePod(ctx, pod.Name, pod.UID, 5)
		p.invalidateRunningPodSnapshot()
		// NotFound and UID-precondition conflicts both prove that this captured
		// immutable object is gone. A replacement, if any, remains untouched.
		if deleteErr == nil || apierrors.IsNotFound(deleteErr) || apierrors.IsConflict(deleteErr) {
			continue
		}
		errs = append(errs, fmt.Errorf("deleting token-matched pod %q (%s): %w", pod.Name, pod.UID, deleteErr))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	fingerprint := capsuleTokenFingerprint(expectedToken)
	return p.deleteCapsuleNetworkPolicies(ctx, label, fingerprint)
}

// Interrupt sends Ctrl-C to the tmux session inside the pod.
func (p *Provider) Interrupt(name string) error {
	_ = p.carrier().Interrupt(context.Background(), name) // best-effort
	return nil
}

// IsRunning reports whether the session has a running pod with a live tmux
// session. A probe failure (timeout, transport error) collapses to false
// here, the same as "confirmed not running" — callers doing destructive
// remediation on the result must use IsRunningChecked instead, so an
// inconclusive probe cannot be mistaken for proof of absence.
func (p *Provider) IsRunning(name string) bool {
	running, _ := p.IsRunningChecked(name)
	return running
}

// IsRunningChecked reports whether the session has a running pod with a live
// tmux session, distinguishing a confirmed negative (no running pod, or a
// tmux session confirmed absent) from a probe that could not complete within
// runningPodSnapshotTimeout — a lookup/exec failure or a deadline — which
// callers must treat as unknown, not as proof of absence.
func (p *Provider) IsRunningChecked(name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		if errors.Is(err, runtime.ErrSessionNotFound) {
			return false, nil
		}
		return false, interactionError("check running", name, err)
	}
	// Pod Running + tmux session alive.
	_, err = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
	if err == nil {
		return true, nil
	}
	var exitErr execerr.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		// Ran and exited non-zero: tmux's own "no such session" result, not
		// a transport failure.
		return false, nil
	}
	return false, interactionError("check running", name, err)
}

// IsAttached reports whether a user terminal is connected to the tmux
// session inside the pod. A probe failure collapses to false here, the same
// as "confirmed not attached" — callers doing destructive remediation on the
// result must use IsAttachedChecked instead.
func (p *Provider) IsAttached(name string) bool {
	attached, _ := p.IsAttachedChecked(name)
	return attached
}

// IsAttachedChecked is the IsAttached analog of IsRunningChecked.
func (p *Provider) IsAttachedChecked(name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		if errors.Is(err, runtime.ErrSessionNotFound) {
			return false, nil
		}
		return false, interactionError("check attached", name, err)
	}
	output, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_attached}"}, nil)
	if err != nil {
		var exitErr execerr.ExitError
		if errors.As(err, &exitErr) && exitErr.Exited() {
			return false, nil
		}
		return false, interactionError("check attached", name, err)
	}
	return strings.TrimSpace(output) == "1", nil
}

type attachCommandRunner func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error

func runKubectlAttach(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// Attach shells out to kubectl exec -it for full TTY passthrough. The remote
// command checks the pod UID projected by the downward API, so a same-name
// replacement between discovery and exec cannot receive terminal input.
func (p *Provider) Attach(name string) error {
	findCtx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	pods, err := p.ops.listPods(findCtx, "gc-session="+SanitizeLabel(name), "status.phase=Running")
	if err != nil {
		return interactionError("attach", name, err)
	}
	var candidates []*corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase != corev1.PodRunning || pod.UID == "" {
			continue
		}
		storedName := pod.Annotations["gc-session-name"]
		if storedName == name || (storedName == "" && pod.Name == SanitizeName(name)) {
			candidates = append(candidates, pod)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("attach session %q: %w", name, runtime.ErrSessionNotFound)
	}
	if len(candidates) != 1 {
		return fmt.Errorf("attach session %q: %w: pod incarnation is ambiguous", name, runtime.ErrRuntimeUnavailable)
	}
	pod := candidates[0]
	podName := pod.Name

	args := []string{}
	if p.k8sContext != "" {
		args = append(args, "--context", p.k8sContext)
	}
	args = append(args, "-n", p.namespace, "exec", "-it", podName, "--",
		"sh", "-c",
		`test "$GC_POD_UID" = "$1" || { echo "stale pod incarnation" >&2; exit 75; }; exec tmux attach -t "$2"`,
		"gc-attach", string(pod.UID), tmuxSession)

	// Interactive attach has no natural deadline: it must run for as long as
	// the user stays attached, not the pod-discovery bound above.
	runner := p.runAttachCommand
	if runner == nil {
		runner = runKubectlAttach
	}
	err = runner(context.Background(), args, p.attachStdin, p.attachStdout, p.attachStderr)
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 75 {
		return fmt.Errorf("attach session %q: %w", name, runtime.ErrInstanceTokenMismatch)
	}
	if err != nil {
		return interactionError("attach", name, err)
	}
	return nil
}

// ProcessAlive checks if the named processes are running inside the pod. A
// probe failure (timeout, transport error) collapses to false here, the same
// as "confirmed not running" — callers doing destructive remediation on the
// result must use ProcessAliveChecked instead, so an inconclusive probe
// cannot be mistaken for proof of death.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	alive, _ := p.ProcessAliveChecked(name, processNames)
	return alive
}

// ProcessAliveChecked reports whether any of processNames is running inside
// the pod for name, distinguishing a confirmed negative (no pod, a pod in
// graceful shutdown or not yet Running, or every pgrep probe exiting
// non-zero) from a probe that could not complete within
// runningPodSnapshotTimeout — a listPods/exec failure or a deadline — which
// callers must treat as unknown, not as proof of death.
func (p *Provider) ProcessAliveChecked(name string, processNames []string) (bool, error) {
	if len(processNames) == 0 {
		return true, nil
	}
	// Reachable from liveness probes and doctor checks on a recurring cadence
	// (internal/runtime/liveness.go, internal/doctor/checks.go); bound it like
	// ListRunning so a wedged API server can't hang that loop (gcy-bru class).
	ctx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	label := SanitizeLabel(name)

	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		return false, interactionError("check process liveness", name, err)
	}
	if len(pods) == 0 {
		return false, nil
	}
	pod := &pods[0]

	// Check deletionTimestamp — pod in graceful shutdown is not alive.
	if pod.DeletionTimestamp != nil {
		return false, nil
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false, nil
	}

	for _, pname := range processNames {
		_, err := p.ops.execInPod(ctx, pod.Name, "agent",
			[]string{"pgrep", "-f", pname}, nil)
		if err == nil {
			return true, nil
		}
		var exitErr execerr.ExitError
		if errors.As(err, &exitErr) && exitErr.Exited() {
			// Ran and exited non-zero: pgrep's own "not found" result for
			// this name, not a transport failure. Keep checking the rest.
			continue
		}
		return false, interactionError("check process liveness", name, err)
	}
	return false, nil
}

// Nudge types a message into the tmux session followed by Enter.
// Uses -l (literal mode) so tmux key names in the message text are not
// interpreted as keystrokes. Content blocks are flattened to text.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	err := p.carrier().Nudge(context.Background(), name, content)
	return interactionError("nudge", name, err)
}

// SendKeys sends bare keystrokes to the tmux session.
func (p *Provider) SendKeys(name string, keys ...string) error {
	err := p.carrier().SendKeys(context.Background(), name, keys...)
	return interactionError("send keys", name, err)
}

// RunLive re-applies session_live commands. Not yet supported for K8s.
func (p *Provider) RunLive(_ string, _ runtime.Config) error {
	return nil
}

// SetMeta stores a key-value pair in the tmux environment.
func (p *Provider) SetMeta(name, key, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	podName, err := p.findPod(ctx, name)
	if err != nil {
		return interactionError("set metadata", name, err)
	}
	_, err = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "set-environment", "-t", tmuxSession, key, value}, nil)
	return interactionError("set metadata", name, err)
}

// GetMeta retrieves a metadata value from the tmux environment.
func (p *Provider) GetMeta(name, key string) (string, error) {
	ctx := context.Background()
	// The instance token is immutable pod-spec identity and is available before
	// tmux starts. Read it from the authoritative object so shutdown cleanup can
	// fence a Pending/initializing pod without treating an exec failure or an
	// empty tmux environment as permission to delete.
	if key == "GC_INSTANCE_TOKEN" {
		label := SanitizeLabel(name)
		pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
		if err != nil {
			return "", interactionError("get metadata", name, fmt.Errorf("%w: list pod for session %q: %w", runtime.ErrRuntimeUnavailable, name, err))
		}
		if len(pods) == 0 {
			return "", interactionError("get metadata", name, fmt.Errorf("%w: no pod for session %q", runtime.ErrSessionNotFound, name))
		}
		var observed string
		for i := range pods {
			token, ok := immutablePodInstanceToken(&pods[i])
			if !ok {
				return "", nil
			}
			if observed == "" {
				observed = token
				continue
			}
			if observed != token {
				return "", interactionError("get metadata", name, fmt.Errorf("%w: multiple pod incarnations have different immutable tokens", runtime.ErrRuntimeUnavailable))
			}
		}
		return observed, nil
	}
	// Non-token fallback: same unbounded-LIST/EXEC risk as the token branch
	// above, bounded the same way.
	ctx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	podName, err := p.findPod(ctx, name)
	if err != nil {
		return "", interactionError("get metadata", name, err)
	}
	output, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "show-environment", "-t", tmuxSession, key}, nil)
	if err != nil {
		// A new tmux session inherits the pod environment into the server's
		// global environment, but it does not copy every inherited variable
		// into the session-specific override table. In that normal case
		// `show-environment -t` exits non-zero with "unknown variable" even
		// though the pane and agent process have the value. Fall back only for
		// that exact absence signal; every real exec/runtime failure remains a
		// typed interaction error.
		if !isTmuxUnknownVariable(err, key) {
			return "", interactionError("get metadata", name, err)
		}
		output, err = p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "show-environment", "-g", key}, nil)
		if err != nil {
			if isTmuxUnknownVariable(err, key) {
				return "", nil
			}
			return "", interactionError("get metadata", name, err)
		}
	}
	output = strings.TrimSpace(output)
	// tmux output: "KEY=VALUE" (set), "-KEY" (unset).
	if output == "-"+key {
		return "", nil // explicitly unset
	}
	if gotKey, val, ok := strings.Cut(output, "="); ok && gotKey == key {
		return val, nil
	}
	return "", fmt.Errorf(
		"%w: k8s get metadata for session %q returned malformed tmux output for key %q",
		runtime.ErrRuntimeUnavailable,
		name,
		key,
	)
}

func immutablePodInstanceToken(pod *corev1.Pod) (string, bool) {
	if pod == nil {
		return "", false
	}
	var token string
	found := false
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name != "agent" {
			continue
		}
		for _, env := range pod.Spec.Containers[i].Env {
			if env.Name != "GC_INSTANCE_TOKEN" {
				continue
			}
			candidate := strings.TrimSpace(env.Value)
			if candidate == "" || (found && candidate != token) {
				return "", false
			}
			token = candidate
			found = true
		}
	}
	return token, found
}

func isTmuxUnknownVariable(err error, key string) bool {
	if err == nil || strings.TrimSpace(key) == "" {
		return false
	}
	return strings.Contains(err.Error(), "unknown variable: "+key)
}

// RemoveMeta removes a metadata key from the tmux environment.
func (p *Provider) RemoveMeta(name, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	podName, err := p.findPod(ctx, name)
	if err != nil {
		return interactionError("remove metadata", name, err)
	}
	_, err = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "set-environment", "-t", tmuxSession, "-u", key}, nil)
	return interactionError("remove metadata", name, err)
}

// Peek captures the last N lines of tmux pane output.
func (p *Provider) Peek(name string, lines int) (string, error) {
	out, err := p.carrier().Peek(context.Background(), name, lines)
	return out, interactionError("peek", name, err)
}

// ListRunning returns names of all running sessions with the given prefix.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()

	if p.runningPodCacheTTL <= 0 {
		return p.listRunningFresh(ctx, prefix)
	}

	snapshot, err := p.runningPodSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range snapshot.agentSessionNames {
		if prefix == "" || strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// ListRunningFresh bypasses the short-lived observation snapshot for lifecycle
// decisions that must see pods which became Running after an earlier list.
func (p *Provider) ListRunningFresh(prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	return p.listRunningFresh(ctx, prefix)
}

func (p *Provider) listRunningFresh(ctx context.Context, prefix string) ([]string, error) {
	pods, err := p.ops.listPods(ctx, "app=gc-agent", "status.phase=Running")
	if err != nil {
		return nil, err
	}
	return runningSessionNames(pods, prefix), nil
}

func runningSessionNames(pods []corev1.Pod, prefix string) []string {
	var names []string
	for i := range pods {
		pod := &pods[i]
		// Prefer annotation (raw name) over label (sanitized).
		name := strings.TrimSpace(pod.Annotations["gc-session-name"])
		if name == "" {
			name = strings.TrimSpace(pod.Labels["gc-session"])
		}
		if name != "" && (prefix == "" || strings.HasPrefix(name, prefix)) {
			names = append(names, name)
		}
	}
	return names
}

// GetLastActivity returns the time of the last I/O in the tmux session.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	// Reachable from the session reconciler and manager on a reconcile
	// cadence (cmd/gc/session_reconciler.go, internal/session/manager.go);
	// bound it like ListRunning so a wedged API server can't hang that loop.
	ctx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return time.Time{}, nil
	}
	output, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_activity}"}, nil)
	if err != nil {
		return time.Time{}, nil
	}
	epoch := strings.TrimSpace(output)
	if epoch == "" {
		return time.Time{}, nil
	}
	secs, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return time.Time{}, nil
	}
	return time.Unix(secs, 0), nil
}

// ClearScrollback clears the tmux scrollback buffer (best-effort).
func (p *Provider) ClearScrollback(name string) error {
	_ = p.carrier().ClearScrollback(context.Background(), name) // best-effort
	return nil
}

// Capabilities reports K8s provider capabilities. The K8s provider
// supports activity tracking via tmux session_activity but does not
// support attachment detection from the controller host.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportActivity: true,
	}
}

// SleepCapability reports that k8s sessions can participate in timed-only
// idle sleep. The controller cannot observe attachment state from the host.
func (p *Provider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}

// CopyTo copies a local file/directory into the pod via tar.
func (p *Provider) CopyTo(name, src, relDst string) error {
	findCtx, cancel := context.WithTimeout(context.Background(), runningPodSnapshotTimeout)
	defer cancel()
	podName, err := p.findRunningPod(findCtx, name)
	if err != nil {
		return nil // best-effort
	}
	dst := "/workspace"
	if relDst != "" {
		dst = "/workspace/" + relDst
	}
	// Work directories can be many gigabytes; the transfer itself must not
	// inherit the pod-discovery bound above.
	return copyToPod(context.Background(), p.ops, podName, "agent", src, dst)
}

// --- Internal helpers ---

// carrier returns the tmux carrier that drives this provider's sessions over
// the pod exec connection ([Provider.Exec]). The in-box tmux session is always
// tmuxSession ("main").
func (p *Provider) carrier() runtime.Carrier {
	return runtime.NewTmuxCarrier(p, tmuxSession)
}

// Exec implements [runtime.ExecProvider]: it runs argv inside the session
// pod's "agent" container and returns the command's standard output and exit
// code (execInPod returns stdout; stderr is folded into err on a transport
// failure). A command that runs but exits non-zero yields that code with a nil
// error; only a transport failure (no running pod, stream error) yields err.
// This is the connection the tmux carrier drives the session through.
func (p *Provider) Exec(ctx context.Context, name string, argv []string) ([]byte, int, error) {
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return nil, -1, fmt.Errorf("k8s exec %q: %w", name, err)
	}
	out, err := p.ops.execInPod(ctx, podName, "agent", argv, nil)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return []byte(out), -1, fmt.Errorf(
				"%w: k8s exec for session %q: %w",
				runtime.ErrSessionNotFound,
				name,
				err,
			)
		}
		var exitErr execerr.ExitError
		if errors.As(err, &exitErr) && exitErr.Exited() {
			// Ran and exited non-zero: the command's own result, not a
			// transport failure (per the ExecProvider contract).
			return []byte(out), exitErr.ExitStatus(), nil
		}
		return []byte(out), -1, fmt.Errorf(
			"%w: k8s exec transport for session %q: %w",
			runtime.ErrRuntimeUnavailable,
			name,
			err,
		)
	}
	return []byte(out), 0, nil
}

// interactionError gives session-scoped interaction failures the shared
// runtime error vocabulary without hiding their provider-specific cause.
func interactionError(op, name string, err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		return fmt.Errorf(
			"%w: k8s %s for session %q: %w",
			runtime.ErrSessionNotFound,
			op,
			name,
			err,
		)
	}
	if errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
		return fmt.Errorf("k8s %s %q: %w", op, name, err)
	}
	return fmt.Errorf(
		"%w: k8s %s for session %q: %w",
		runtime.ErrRuntimeUnavailable,
		op,
		name,
		err,
	)
}

// findRunningPod finds a running pod by session label.
func (p *Provider) findRunningPod(ctx context.Context, name string) (string, error) {
	if p.runningPodCacheTTL > 0 {
		return p.findRunningPodFromSnapshot(ctx, name)
	}
	label := SanitizeLabel(name)
	pods, err := p.ops.listPods(ctx, "gc-session="+label, "status.phase=Running")
	if err != nil {
		return "", fmt.Errorf(
			"%w: list running pod for session %q: %w",
			runtime.ErrRuntimeUnavailable,
			name,
			err,
		)
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("%w: no running pod for session %q", runtime.ErrSessionNotFound, name)
	}
	return pods[0].Name, nil
}

// findRunningPodFromSnapshot amortizes live-state reads across a whole
// reconciliation/dashboard burst. Enriching one session asks for running,
// attachment, and activity independently, and list pages repeat that for every
// session. A per-name Kubernetes LIST turns that into enough client-go
// throttling to delay an SSE response before it has even written headers. One
// short-lived namespace snapshot preserves current state while reducing the
// hot path to a single LIST shared by all session names.
func (p *Provider) findRunningPodFromSnapshot(ctx context.Context, name string) (string, error) {
	snapshot, err := p.runningPodSnapshot(ctx)
	if err != nil {
		return "", err
	}

	podName := snapshot.bySession[name]
	if podName == "" {
		// Labels are sanitized for Kubernetes, while annotations preserve the
		// original runtime name. Legacy pods may have only the label.
		podName = snapshot.bySession[SanitizeLabel(name)]
	}
	if podName == "" {
		// Confirmed absent from a freshly-listed snapshot, not a lookup
		// failure — wrap the same sentinel the non-snapshot path uses so
		// callers (e.g. IsRunningChecked) can tell this apart from a
		// snapshot refresh that itself failed to complete.
		return "", fmt.Errorf("%w: no running pod for session %q", runtime.ErrSessionNotFound, name)
	}
	return podName, nil
}

// runningPodSnapshot returns one short-lived namespace snapshot shared by
// every list and per-session lookup in a controller burst. Refresh failures
// are coalesced but never cached, and the shared Kubernetes request has its
// own bound so it does not inherit the winning caller's lifetime.
func (p *Provider) runningPodSnapshot(ctx context.Context) (*runningPodState, error) {
	if snapshot, ok := p.cachedRunningPodSnapshot(time.Now()); ok {
		return snapshot, nil
	}

	result := p.runningPodFlight.DoChan("refresh", func() (any, error) {
		snapshot, ok, generation := p.cachedRunningPodSnapshotForRefresh(time.Now())
		if ok {
			return snapshot, nil
		}

		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runningPodSnapshotTimeout)
		defer cancel()
		pods, err := p.ops.listPods(refreshCtx, "", "status.phase=Running")
		if err != nil {
			return nil, err
		}
		snapshot = &runningPodState{bySession: make(map[string]string, len(pods))}
		for i := range pods {
			sessionName := strings.TrimSpace(pods[i].Annotations["gc-session-name"])
			if sessionName == "" {
				sessionName = strings.TrimSpace(pods[i].Labels["gc-session"])
			}
			if sessionName == "" {
				continue
			}
			if _, exists := snapshot.bySession[sessionName]; !exists {
				snapshot.bySession[sessionName] = pods[i].Name
			}
			if pods[i].Labels["app"] == "gc-agent" {
				snapshot.agentSessionNames = append(snapshot.agentSessionNames, sessionName)
			}
		}

		p.runningPodCacheMu.Lock()
		if p.runningPodCacheGen == generation {
			p.runningPodCache = snapshot
			p.runningPodCacheAt = time.Now()
		}
		p.runningPodCacheMu.Unlock()
		return snapshot, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case refresh := <-result:
		if refresh.Err != nil {
			return nil, refresh.Err
		}
		snapshot, ok := refresh.Val.(*runningPodState)
		if !ok {
			return nil, fmt.Errorf("running-pod snapshot returned %T", refresh.Val)
		}
		return snapshot, nil
	}
}

func (p *Provider) cachedRunningPodSnapshot(now time.Time) (*runningPodState, bool) {
	p.runningPodCacheMu.RLock()
	defer p.runningPodCacheMu.RUnlock()
	if p.runningPodCache == nil || now.Sub(p.runningPodCacheAt) >= p.runningPodCacheTTL {
		return nil, false
	}
	return p.runningPodCache, true
}

func (p *Provider) cachedRunningPodSnapshotForRefresh(now time.Time) (*runningPodState, bool, uint64) {
	p.runningPodCacheMu.RLock()
	defer p.runningPodCacheMu.RUnlock()
	if p.runningPodCache == nil || now.Sub(p.runningPodCacheAt) >= p.runningPodCacheTTL {
		return nil, false, p.runningPodCacheGen
	}
	return p.runningPodCache, true, p.runningPodCacheGen
}

// invalidateRunningPodSnapshot establishes a mutation generation boundary.
// Forget runs while the cache lock is held so a post-mutation caller cannot
// join the pre-mutation singleflight. A pre-mutation refresh may still return to
// its original callers, but its generation check prevents it from republishing
// stale state into the cache.
func (p *Provider) invalidateRunningPodSnapshot() {
	p.runningPodCacheMu.Lock()
	p.runningPodCacheGen++
	p.runningPodCache = nil
	p.runningPodCacheAt = time.Time{}
	p.runningPodFlight.Forget("refresh")
	p.runningPodCacheMu.Unlock()
}

// findPod finds a pod by session label (any phase).
func (p *Provider) findPod(ctx context.Context, name string) (string, error) {
	label := SanitizeLabel(name)
	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		return "", fmt.Errorf(
			"%w: list pod for session %q: %w",
			runtime.ErrRuntimeUnavailable,
			name,
			err,
		)
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("%w: no pod for session %q", runtime.ErrSessionNotFound, name)
	}
	return pods[0].Name, nil
}

// waitForDeletion waits for a pod to be deleted.
func waitForDeletion(ctx context.Context, ops k8sOps, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := ops.getPod(ctx, name)
		if err != nil {
			return nil // gone
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("pod %s not deleted after %s", name, timeout)
}

// waitForPodRunning waits for the pod to reach Running phase.
func waitForPodRunning(ctx context.Context, ops k8sOps, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		pod, err := ops.getPod(ctx, name)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("pod %s failed", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("pod %s not running after %s", name, timeout)
}

// waitForTmux waits for the tmux session to be available inside the pod.
func waitForTmux(ctx context.Context, ops k8sOps, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := ops.execInPod(ctx, name, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("tmux session not ready in pod %s after %s", name, timeout)
}

// initCityInPod copies the city directory, materializes local packs omitted by
// gc init --from, and installs locked remote imports before releasing the
// worker. A partially initialized city cannot run gc hook reliably.
func initCityInPod(ctx context.Context, ops k8sOps, podName, ctrlCity string) error {
	// Copy only the authored city definition. The live city also contains
	// mutable runtime, Dolt, backup, state, and log trees that are neither
	// needed nor safe to clone into an isolated worker.
	if err := copyCitySourceToPod(ctx, ops, podName, "agent", ctrlCity, "/tmp/city-src"); err != nil {
		return err
	}
	defer func() {
		_, _ = ops.execInPod(ctx, podName, "agent",
			[]string{"rm", "-rf", "/tmp/city-src"}, nil)
	}()

	// gc init resolves rig imports while loading the source city, before it
	// scaffolds the destination. Preseed authored local packs so imports such
	// as ./packs/rig-basic are available during that resolution.
	_, err := ops.execInPod(ctx, podName, "agent", []string{
		"sh", "-c",
		"if [ -d /tmp/city-src/packs ]; then " +
			"mkdir -p /workspace/packs && " +
			"cp -a /tmp/city-src/packs/. /workspace/packs/; " +
			"fi",
	}, nil)
	if err != nil {
		return fmt.Errorf("materializing local city packs: %w", err)
	}

	// Run gc init --from with GC_DOLT=skip so gc init does not attempt to
	// start a local Dolt server. Skip provider readiness while materializing the
	// shared city because an isolated worker pod only receives credentials for
	// its selected provider; the provider-specific agent startup remains the
	// authoritative readiness check. Pod sessions consume the projected
	// GC_DOLT_* connection target through env; they do not rewrite canonical
	// .beads files.
	_, err = ops.execInPod(ctx, podName, "agent",
		[]string{
			"env", "GC_DOLT=skip",
			"gc", "init", "--skip-provider-readiness", "--no-start",
			"--from", "/tmp/city-src", "/workspace",
		}, nil)
	if err != nil {
		return err
	}

	// Locked remote imports are stored outside the city tree under the worker
	// home. Install them while startup credentials are available and before
	// .gc-workspace-ready lets pre_start or the agent invoke gc hook.
	_, err = ops.execInPod(ctx, podName, "agent",
		[]string{"gc", "--city", "/workspace", "import", "install"}, nil)
	if err != nil {
		return fmt.Errorf("installing locked city imports: %w", err)
	}
	return projectCityIdentityInPod(ctx, ops, podName, ctrlCity)
}

// projectCityIdentityInPod restores the controller-validated database identity
// after gc init materializes the isolated city. Authored city staging excludes
// .beads by design, so without this narrow projection the generated
// /workspace/.beads/metadata.json has no project_id and native-store preflight
// falls back even though the controller already established canonical L1
// identity. Endpoint and credential fields remain pod-local.
func projectCityIdentityInPod(ctx context.Context, ops k8sOps, podName, ctrlCity string) error {
	raw, err := os.ReadFile(filepath.Join(ctrlCity, ".beads", "metadata.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading canonical city metadata: %w", err)
	}
	var metadata struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return fmt.Errorf("parsing canonical city metadata: %w", err)
	}
	projectID := strings.TrimSpace(metadata.ProjectID)
	if projectID == "" {
		return nil
	}
	projectIDB64 := base64.StdEncoding.EncodeToString([]byte(projectID))
	script := fmt.Sprintf(
		`PROJECT_ID=$(echo '%s' | base64 -d) && `+
			`python3 -c "import json,sys; p=sys.argv[1]; `+
			`m=json.load(open(p)); m['project_id']=sys.argv[2]; `+
			`json.dump(m,open(p,'w'),indent=2)" `+
			`/workspace/.beads/metadata.json "$PROJECT_ID"`,
		projectIDB64,
	)
	if _, err := ops.execInPod(ctx, podName, "agent", []string{"sh", "-c", script}, nil); err != nil {
		return fmt.Errorf("projecting canonical city identity: %w", err)
	}
	return nil
}

// initBeadsInPod ensures the pod workspace has usable .beads state. It keeps
// the older warning-only self-heal behavior for prebaked or older staged
// workspaces by patching existing metadata and bootstrapping missing state.
func initBeadsInPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, workDir, managedServiceHost, managedServicePort string) error {
	projected, err := projectedPodDoltEnv(cfg.Env, managedServiceHost, managedServicePort)
	if err != nil {
		return err
	}
	if len(projected) == 0 {
		return nil
	}
	doltHost := projected["GC_DOLT_HOST"]
	doltPort := projected["GC_DOLT_PORT"]
	storeRoot := projectedPodStoreRoot(cfg, workDir)
	prefix := strings.TrimSpace(cfg.Env["GC_BEADS_PREFIX"])
	if prefix == "" {
		return fmt.Errorf("missing projected GC_BEADS_PREFIX")
	}

	portNum, err := strconv.Atoi(doltPort)
	if err != nil {
		return fmt.Errorf("invalid projected GC_DOLT_PORT %q: %w", doltPort, err)
	}
	patchJSON, err := json.Marshal(map[string]any{
		"dolt_server_host": doltHost,
		"dolt_server_port": portNum,
	})
	if err != nil {
		return fmt.Errorf("marshaling beads patch: %w", err)
	}
	patchB64 := base64.StdEncoding.EncodeToString(patchJSON)
	prefixB64 := base64.StdEncoding.EncodeToString([]byte(prefix))
	storeRootB64 := base64.StdEncoding.EncodeToString([]byte(storeRoot))

	patchCmd := fmt.Sprintf(
		`WD=$(echo '%s' | base64 -d) && cd "$WD" && PATCH=$(echo '%s' | base64 -d) && `+
			`if [ -f .beads/metadata.json ]; then `+
			`python3 -c "import json,sys; `+
			`m=json.load(open('.beads/metadata.json')); `+
			`p=json.loads(sys.argv[1]); m.update(p); `+
			`json.dump(m,open('.beads/metadata.json','w'),indent=2)" "$PATCH" 2>/dev/null || `+
			`printf '%%s' "$PATCH" | python3 -c "import json,sys; `+
			`m=json.load(open('.beads/metadata.json')); `+
			`p=json.loads(sys.stdin.read()); m.update(p); `+
			`json.dump(m,open('.beads/metadata.json','w'),indent=2)"; `+
			`else PREFIX=$(echo '%s' | base64 -d) && `+
			`DOLT_HOST=$(echo '%s' | base64 -d) && `+
			`DOLT_PORT=$(echo '%s' | base64 -d) && `+
			`yes | BEADS_DIR="$WD/.beads" bd init --server --server-host "$DOLT_HOST" --server-port "$DOLT_PORT" -p "$PREFIX" --skip-hooks --skip-agents; fi`,
		storeRootB64, patchB64, prefixB64,
		base64.StdEncoding.EncodeToString([]byte(doltHost)),
		base64.StdEncoding.EncodeToString([]byte(doltPort)),
	)
	_, err = ops.execInPod(ctx, podName, "agent", []string{"sh", "-c", patchCmd}, nil)
	return err
}

// verifyBeadsInPod confirms that canonical tracked .beads files are already
// present in the mounted workspace for bd-backed sessions. It intentionally
// does not create or rewrite .beads state inside the pod.
//
//nolint:unparam // tests exercise this helper through the canonical managed service constants.
func verifyBeadsInPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, storeRoot, managedServiceHost, managedServicePort string) error {
	projected, err := projectedPodDoltEnv(cfg.Env, managedServiceHost, managedServicePort)
	if err != nil {
		return err
	}
	if len(projected) == 0 {
		return nil
	}
	_, err = ops.execInPod(ctx, podName, "agent", []string{
		"sh", "-c",
		`cd "$1" && test -f .beads/metadata.json && test -f .beads/config.yaml`,
		"sh", storeRoot,
	}, nil)
	if err != nil {
		return fmt.Errorf("canonical .beads files missing or unreadable at %s: %w", storeRoot, err)
	}
	return nil
}

func buildRESTConfig(k8sContext string) (*rest.Config, error) {
	// Try in-cluster first.
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	// Fall back to kubeconfig.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if k8sContext != "" {
		overrides.CurrentContext = k8sContext
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func managedServiceAlias() (string, string, error) {
	host := strings.TrimSpace(os.Getenv("GC_K8S_DOLT_HOST"))
	port := strings.TrimSpace(os.Getenv("GC_K8S_DOLT_PORT"))
	switch {
	case host == "" && port == "":
		return podManagedDoltHost, podManagedDoltPort, nil
	case host == "" || port == "":
		return "", "", fmt.Errorf("requires both GC_K8S_DOLT_HOST and GC_K8S_DOLT_PORT when either is set")
	default:
		return host, port, nil
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
