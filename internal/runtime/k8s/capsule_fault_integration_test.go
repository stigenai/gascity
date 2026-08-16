//go:build integration

package k8s

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestKindCapsuleReplacementCleanupAndForeignResourceFence(t *testing.T) {
	if os.Getenv("GC_K8S_FAULT_TEST") != "1" {
		t.Skip("requires GC_K8S_FAULT_TEST=1 for a disposable Kind namespace")
	}
	loading := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	)
	raw, err := loading.RawConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	if !strings.HasPrefix(raw.CurrentContext, "kind-") {
		t.Skipf("current Kubernetes context %q is not an isolated Kind cluster", raw.CurrentContext)
	}
	restConfig, err := loading.ClientConfig()
	if err != nil {
		t.Fatalf("build Kubernetes client config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("build Kubernetes client: %v", err)
	}

	namespace := fmt.Sprintf("gc-capsule-fault-test-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create isolated namespace %q: %v", namespace, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := clientset.CoreV1().Namespaces().Delete(cleanupCtx, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete isolated namespace %q: %v", namespace, err)
			return
		}
		if err := wait.PollUntilContextTimeout(cleanupCtx, 50*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		}); err != nil {
			t.Errorf("wait for isolated namespace %q deletion: %v", namespace, err)
		}
	})

	realOps := &realK8sOps{clientset: clientset, restConfig: restConfig, namespace: namespace}
	provider := newProviderWithOps(realOps)
	provider.namespace = namespace
	provider.capsuleCityScope = "kind/" + namespace + "/city"
	key, err := runtime.NewCapsuleKey(provider.capsuleCityScope, "ga-kind-replacement")
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := provider.EnsureCapsuleState(ctx, key)
	if err != nil {
		t.Fatalf("ensure retained state: %v", err)
	}
	capsule := &runtime.CapsuleLaunchConfig{Key: key, State: ref, Network: runtime.CapsuleNetworkOffline}
	cfg := runtime.Config{Capsule: capsule, Env: map[string]string{"GC_INSTANCE_TOKEN": "kind-fault-instance"}}
	policyRef, _, err := provider.ensureCapsuleNetworkPolicy(ctx, key.SessionID, cfg)
	if err != nil {
		t.Fatalf("ensure NetworkPolicy: %v", err)
	}

	ownPod := kindCapsuleFixturePod(namespace, key.SessionID, "kind-owned-pod", key, ref.ResourceID)
	createdOwnPod, err := realOps.createPod(ctx, ownPod)
	if err != nil {
		t.Fatalf("create owned pod: %v", err)
	}
	if err := realOps.deletePod(ctx, createdOwnPod.Name, createdOwnPod.UID, 0); err != nil {
		t.Fatalf("inject pod eviction: %v", err)
	}
	waitForKindPodAbsent(t, ctx, realOps, createdOwnPod.Name)
	replacement := kindCapsuleFixturePod(namespace, key.SessionID, "kind-replacement-pod", key, ref.ResourceID)
	createdReplacement, err := realOps.createPod(ctx, replacement)
	if err != nil {
		t.Fatalf("create replacement pod: %v", err)
	}
	reopened, ok, err := provider.OpenCapsuleState(ctx, key)
	if err != nil || !ok || reopened != ref {
		t.Fatalf("retained state after replacement = %#v, %t, %v; want %#v", reopened, ok, err, ref)
	}

	foreignScope := "kind/" + namespace + "/another-city"
	foreignKey, err := runtime.NewCapsuleKey(foreignScope, key.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	foreignPVC, err := clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, kindCapsuleFixturePVC(namespace, foreignKey), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create foreign PVC: %v", err)
	}
	foreignPod, err := realOps.createPod(ctx, kindCapsuleFixturePod(namespace, key.SessionID, "kind-foreign-pod", foreignKey, foreignPVC.Name))
	if err != nil {
		t.Fatalf("create foreign pod: %v", err)
	}
	foreignPolicy, err := realOps.createNetworkPolicy(ctx, kindCapsuleFixturePolicy(namespace, key.SessionID, foreignKey))
	if err != nil {
		t.Fatalf("create foreign NetworkPolicy: %v", err)
	}

	if err := provider.Stop(key.SessionID); err != nil {
		t.Fatalf("stop owned capsule resources: %v", err)
	}
	waitForKindPodAbsent(t, ctx, realOps, createdReplacement.Name)
	if _, err := realOps.getPod(ctx, foreignPod.Name); err != nil {
		t.Fatalf("foreign pod did not survive owned cleanup: %v", err)
	}
	if _, err := realOps.getNetworkPolicy(ctx, foreignPolicy.Name); err != nil {
		t.Fatalf("foreign NetworkPolicy did not survive owned cleanup: %v", err)
	}
	if _, err := realOps.getPVC(ctx, foreignPVC.Name); err != nil {
		t.Fatalf("foreign PVC did not survive owned cleanup: %v", err)
	}
	if _, err := realOps.getNetworkPolicy(ctx, policyRef.Name); !apierrors.IsNotFound(err) {
		t.Fatalf("owned NetworkPolicy after Stop = %v, want NotFound", err)
	}
	if err := provider.PurgeCapsuleState(ctx, key); err != nil {
		t.Fatalf("purge retained state: %v", err)
	}

	if err := realOps.deletePod(ctx, foreignPod.Name, foreignPod.UID, 0); err != nil {
		t.Fatalf("delete foreign fixture pod: %v", err)
	}
	waitForKindPodAbsent(t, ctx, realOps, foreignPod.Name)
	if err := realOps.deleteNetworkPolicy(ctx, foreignPolicy.Name, foreignPolicy.UID); err != nil {
		t.Fatalf("delete foreign fixture NetworkPolicy: %v", err)
	}
	if err := realOps.deletePVC(ctx, foreignPVC.Name, foreignPVC.UID); err != nil {
		t.Fatalf("delete foreign fixture PVC: %v", err)
	}
	assertNoKindCapsuleResources(t, ctx, realOps)
}

func kindCapsuleFixturePod(namespace, session, name string, key runtime.CapsuleKey, claim string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{"app": "gc-agent", "gc-session": SanitizeLabel(session), "gc-capsule": "true"},
			Annotations: map[string]string{
				"gc-capsule-digest":        key.Digest,
				capsuleCityScopeAnnotation: capsuleCityScopeFingerprint(key.CityScope),
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "fixture", Image: "registry.k8s.io/pause:3.9",
				VolumeMounts: []corev1.VolumeMount{{Name: "capsule-state", MountPath: capsuleStateMountPath}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "capsule-state",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}},
			}},
		},
	}
}

func kindCapsuleFixturePVC(namespace string, key runtime.CapsuleKey) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: key.ResourceStem(), Namespace: namespace,
			Labels:      map[string]string{capsuleStateLabel: "true", capsuleTokenLabel: key.Token},
			Annotations: capsuleStateAnnotations(key),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("16Mi"),
			}},
		},
	}
}

func kindCapsuleFixturePolicy(namespace, session string, key runtime.CapsuleKey) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kind-foreign-policy", Namespace: namespace,
			Labels: map[string]string{capsuleNetworkPolicyLabel: "true", "gc-session": SanitizeLabel(session)},
			Annotations: map[string]string{
				capsuleCityScopeAnnotation: capsuleCityScopeFingerprint(key.CityScope),
				capsuleInstanceAnnotation:  capsuleTokenFingerprint("foreign-instance"),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"gc-session": SanitizeLabel(session), "gc-capsule": "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

func waitForKindPodAbsent(t *testing.T, ctx context.Context, ops *realK8sOps, name string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := ops.getPod(ctx, name)
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Fatalf("wait for pod %q deletion: %v", name, err)
	}
}

func assertNoKindCapsuleResources(t *testing.T, ctx context.Context, ops *realK8sOps) {
	t.Helper()
	pods, err := ops.listPods(ctx, "gc-capsule=true", "")
	if err != nil {
		t.Fatal(err)
	}
	policies, err := ops.listNetworkPolicies(ctx, capsuleNetworkPolicyLabel+"=true")
	if err != nil {
		t.Fatal(err)
	}
	pvcs, err := ops.listPVCs(ctx, capsuleStateLabel+"=true")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 0 || len(policies) != 0 || len(pvcs) != 0 {
		t.Fatalf("leaked capsule resources: pods=%d policies=%d PVCs=%d", len(pods), len(policies), len(pvcs))
	}
}
