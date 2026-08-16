package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBuildCapsuleNetworkPolicyDeniesIngressAndUsesOnlyNamedEgressPeers(t *testing.T) {
	t.Parallel()
	provider := newProviderWithOps(newFakeK8sOps())
	for _, mode := range []runtime.CapsuleNetworkMode{runtime.CapsuleNetworkOffline, runtime.CapsuleNetworkExternalModel} {
		t.Run(string(mode), func(t *testing.T) {
			capsule := testK8sCapsuleLaunch(t)
			capsule.Network = mode
			policy, err := provider.buildCapsuleNetworkPolicy("ga-network", runtime.Config{Capsule: capsule})
			if err != nil {
				t.Fatal(err)
			}
			if len(policy.Spec.Ingress) != 0 || len(policy.Spec.PolicyTypes) != 2 {
				t.Fatalf("default-deny ingress policy = %#v", policy.Spec)
			}
			if policy.Spec.PodSelector.MatchLabels["gc-capsule"] != "true" || policy.Spec.PodSelector.MatchLabels["gc-session"] != "ga-network" {
				t.Fatalf("pod selector = %#v", policy.Spec.PodSelector)
			}
			text := networkPolicyText(policy)
			for _, forbidden := range []string{"0.0.0.0/0", "::/0", "169.254.169.254", "kubernetes.default", "controller"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("policy permits forbidden destination %q: %s", forbidden, text)
				}
			}
			wantGateway := mode == runtime.CapsuleNetworkExternalModel
			if strings.Contains(text, `"`+modelEgressLabel+`":"`+modelEgressValue+`"`) != wantGateway {
				t.Fatalf("gateway egress for %s = %s", mode, text)
			}
			for _, required := range []string{"kube-system", "kube-dns", `"app":"dolt"`, provider.managedServicePort} {
				if !strings.Contains(text, required) {
					t.Fatalf("policy missing %q: %s", required, text)
				}
			}
		})
	}
}

func TestProviderCapsuleExternalNetworkRequiresGatewayBeforeMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	provider.prebaked = true
	provider.postStartSettle = 0
	capsule := testK8sCapsuleLaunch(t)
	ref, _, err := provider.EnsureCapsuleState(ctx, capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = ref
	cfg := runtime.Config{Capsule: capsule, Env: map[string]string{"GC_INSTANCE_TOKEN": "instance-a"}}
	if err := provider.Start(ctx, "ga-network", cfg); err == nil || !strings.Contains(err.Error(), modelEgressLabel+"="+modelEgressValue) {
		t.Fatalf("missing gateway error = %v", err)
	}
	assertNoPodOrPolicyMutation(t, ops)

	ops.pods["model-gateway"] = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "model-gateway", Labels: map[string]string{modelEgressLabel: modelEgressValue}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	ops.setExecResult("ga-network", []string{"tmux", "has-session", "-t", tmuxSession}, "", nil)
	if err := provider.Start(ctx, "ga-network", cfg); err != nil {
		t.Fatalf("Start with gateway: %v", err)
	}
	if len(ops.networkPolicies) != 1 {
		t.Fatalf("NetworkPolicy count = %d, want one", len(ops.networkPolicies))
	}
	if err := provider.Stop("ga-network"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(ops.networkPolicies) != 0 {
		t.Fatalf("Stop retained NetworkPolicies: %#v", ops.networkPolicies)
	}
	if _, ok := ops.pvcs[ref.ResourceID]; !ok {
		t.Fatal("Stop deleted retained capsule state")
	}
}

func TestProviderCapsuleNetworkPolicyFailurePreventsPodCreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	ref, _, err := provider.EnsureCapsuleState(ctx, capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = ref
	ops.networkCreateErr = errors.New("network policy admission denied")
	err = provider.Start(ctx, "ga-network-failure", runtime.Config{Capsule: capsule})
	if err == nil || !strings.Contains(err.Error(), "network policy admission denied") {
		t.Fatalf("Start error = %v", err)
	}
	for _, call := range ops.calls {
		if call.method == "createPod" {
			t.Fatalf("pod created after policy failure: %#v", ops.calls)
		}
	}
}

func TestEnsureCapsuleNetworkPolicyConvergesAndRejectsOwnedConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	cfg := runtime.Config{Capsule: capsule, Env: map[string]string{"GC_INSTANCE_TOKEN": "instance-concurrent"}}

	const contenders = 12
	errs := make(chan error, contenders)
	var wait sync.WaitGroup
	for range contenders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, err := provider.ensureCapsuleNetworkPolicy(ctx, "ga-network-concurrent", cfg)
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent network policy ensure: %v", err)
		}
	}
	if len(ops.networkPolicies) != 1 {
		t.Fatalf("NetworkPolicy count = %d, want one", len(ops.networkPolicies))
	}
	for _, policy := range ops.networkPolicies {
		policy.Spec.Egress = append(policy.Spec.Egress, policy.Spec.Egress[0])
	}
	if _, _, err := provider.ensureCapsuleNetworkPolicy(ctx, "ga-network-concurrent", cfg); err == nil || !strings.Contains(err.Error(), "conflicts with required isolation") {
		t.Fatalf("conflicting policy error = %v", err)
	}
}

func TestProviderCapsulePodCreateFailureRemovesNewNetworkPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	ref, _, err := provider.EnsureCapsuleState(ctx, capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = ref
	ops.createErr = errors.New("pod admission denied")
	err = provider.Start(ctx, "ga-pod-failure", runtime.Config{Capsule: capsule})
	if err == nil || !strings.Contains(err.Error(), "pod admission denied") {
		t.Fatalf("Start error = %v", err)
	}
	if len(ops.networkPolicies) != 0 {
		t.Fatalf("failed pod left NetworkPolicy: %#v", ops.networkPolicies)
	}
}

func TestBuildPodCapsuleHasNoPublicOrHostBoundaryAndSurfacesDynamicException(t *testing.T) {
	t.Parallel()
	capsule := testK8sCapsuleLaunch(t)
	pod, err := buildPod("ga-contained", runtime.Config{Capsule: capsule, Env: map[string]string{"LINUX_USERNAME": "capsule-user"}}, newProviderWithOps(newFakeK8sOps()))
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC || len(pod.Spec.Containers[0].Ports) != 0 {
		t.Fatalf("capsule requested host/public network surface: %#v", pod.Spec)
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			t.Fatalf("capsule uses hostPath volume: %#v", volume)
		}
	}
	if pod.Annotations["gascity.dev/security-exception"] != "dynamic-user-bootstrap" {
		t.Fatalf("dynamic-user exception annotation = %q", pod.Annotations["gascity.dev/security-exception"])
	}
	security := pod.Spec.Containers[0].SecurityContext
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.SeccompProfile == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("dynamic capsule security context = %#v", security)
	}
	if strings.Contains(strings.Join(pod.Spec.Containers[0].Args, " "), "NOPASSWD") {
		t.Fatal("capsule dynamic user retained passwordless sudo")
	}
}

func assertNoPodOrPolicyMutation(t *testing.T, ops *fakeK8sOps) {
	t.Helper()
	for _, call := range ops.calls {
		if call.method == "createPod" || call.method == "deletePod" || call.method == "createNetworkPolicy" || call.method == "deleteNetworkPolicy" {
			t.Fatalf("network preflight mutated Kubernetes: %#v", ops.calls)
		}
	}
}

func networkPolicyText(policy any) string {
	data, _ := json.Marshal(policy)
	return string(data)
}
