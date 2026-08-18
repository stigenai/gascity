package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// k8sOps abstracts Kubernetes API calls for testability.
// Same pattern as tmux provider's startOps: separates API calls from
// provider logic so unit tests use a fake implementation.
type k8sOps interface {
	createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error)
	getPod(ctx context.Context, name string) (*corev1.Pod, error)
	deletePod(ctx context.Context, name string, uid types.UID, grace int64) error
	listPods(ctx context.Context, selector string, fieldSelector string) ([]corev1.Pod, error)
	execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error)
}

type k8sPVCOps interface {
	createPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error)
	getPVC(ctx context.Context, name string) (*corev1.PersistentVolumeClaim, error)
	listPVCs(ctx context.Context, selector string) ([]corev1.PersistentVolumeClaim, error)
	deletePVC(ctx context.Context, name string, uid types.UID) error
}

type k8sConfigMapOps interface {
	createConfigMap(ctx context.Context, configMap *corev1.ConfigMap) (*corev1.ConfigMap, error)
	getConfigMap(ctx context.Context, name string) (*corev1.ConfigMap, error)
	deleteConfigMap(ctx context.Context, name string, uid types.UID) error
}

type k8sNetworkOps interface {
	createNetworkPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error)
	getNetworkPolicy(ctx context.Context, name string) (*networkingv1.NetworkPolicy, error)
	listNetworkPolicies(ctx context.Context, selector string) ([]networkingv1.NetworkPolicy, error)
	deleteNetworkPolicy(ctx context.Context, name string, uid types.UID) error
}

// realK8sOps wraps a Kubernetes clientset and REST config for real API calls.
type realK8sOps struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
	namespace  string
}

func (r *realK8sOps) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	return r.clientset.CoreV1().Pods(r.namespace).Create(ctx, pod, metav1.CreateOptions{})
}

func (r *realK8sOps) getPod(ctx context.Context, name string) (*corev1.Pod, error) {
	return r.clientset.CoreV1().Pods(r.namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *realK8sOps) deletePod(ctx context.Context, name string, uid types.UID, grace int64) error {
	if uid == "" {
		return fmt.Errorf("refusing to delete pod %q without an immutable UID", name)
	}
	return r.clientset.CoreV1().Pods(r.namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		Preconditions:      &metav1.Preconditions{UID: &uid},
	})
}

func (r *realK8sOps) listPods(ctx context.Context, selector string, fieldSelector string) ([]corev1.Pod, error) {
	opts := metav1.ListOptions{LabelSelector: selector}
	if fieldSelector != "" {
		opts.FieldSelector = fieldSelector
	}
	list, err := r.clientset.CoreV1().Pods(r.namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realK8sOps) createPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	return r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Create(ctx, pvc, metav1.CreateOptions{})
}

func (r *realK8sOps) getPVC(ctx context.Context, name string) (*corev1.PersistentVolumeClaim, error) {
	return r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *realK8sOps) listPVCs(ctx context.Context, selector string) ([]corev1.PersistentVolumeClaim, error) {
	list, err := r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realK8sOps) deletePVC(ctx context.Context, name string, uid types.UID) error {
	if uid == "" {
		return fmt.Errorf("refusing to delete PVC %q without an immutable UID", name)
	}
	return r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
}

func (r *realK8sOps) createConfigMap(ctx context.Context, configMap *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	return r.clientset.CoreV1().ConfigMaps(r.namespace).Create(ctx, configMap, metav1.CreateOptions{})
}

func (r *realK8sOps) getConfigMap(ctx context.Context, name string) (*corev1.ConfigMap, error) {
	return r.clientset.CoreV1().ConfigMaps(r.namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *realK8sOps) deleteConfigMap(ctx context.Context, name string, uid types.UID) error {
	if uid == "" {
		return fmt.Errorf("refusing to delete ConfigMap %q without an immutable UID", name)
	}
	return r.clientset.CoreV1().ConfigMaps(r.namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
}

func (r *realK8sOps) createNetworkPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error) {
	return r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Create(ctx, policy, metav1.CreateOptions{})
}

func (r *realK8sOps) getNetworkPolicy(ctx context.Context, name string) (*networkingv1.NetworkPolicy, error) {
	return r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *realK8sOps) listNetworkPolicies(ctx context.Context, selector string) ([]networkingv1.NetworkPolicy, error) {
	list, err := r.clientset.NetworkingV1().NetworkPolicies(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realK8sOps) deleteNetworkPolicy(ctx context.Context, name string, uid types.UID) error {
	if uid == "" {
		return fmt.Errorf("refusing to delete NetworkPolicy %q without an immutable UID", name)
	}
	return r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
}

func (r *realK8sOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	req := r.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(pod).
		Namespace(r.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("creating SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	streamOpts := remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}
	if stdin != nil {
		streamOpts.Stdin = stdin
	}

	if err := exec.StreamWithContext(ctx, streamOpts); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return stdout.String(), fmt.Errorf("exec in pod %s: %s: %w", pod, errMsg, err)
		}
		return stdout.String(), fmt.Errorf("exec in pod %s: %w", pod, err)
	}
	return stdout.String(), nil
}

// fakeK8sOps is an in-memory test double with spy capabilities.
// Records all calls for assertions and returns configurable results.
type fakeK8sOps struct {
	pods            map[string]*corev1.Pod
	pvcs            map[string]*corev1.PersistentVolumeClaim
	configMaps      map[string]*corev1.ConfigMap
	networkPolicies map[string]*networkingv1.NetworkPolicy
	calls           []fakeCall
	pvcMu           sync.Mutex

	// Configurable behaviors.
	execOutput         map[string]string                              // pod+cmd key → stdout
	execErr            map[string]error                               // pod+cmd key → error
	execFunc           func(pod string, cmd []string) (string, error) // dynamic override, checked first
	createErr          error
	deleteErr          error
	getErr             error
	listErr            error
	pvcCreateErr       error
	pvcGetErr          error
	pvcListErr         error
	pvcDeleteErr       error
	configMapCreateErr error
	configMapGetErr    error
	configMapDeleteErr error
	networkCreateErr   error
	networkGetErr      error
	networkListErr     error
	networkDeleteErr   error
	beforeDelete       func(name string)
}

type fakeCall struct {
	method    string
	pod       string
	uid       types.UID
	container string
	cmd       []string
	selector  string
}

func newFakeK8sOps() *fakeK8sOps {
	return &fakeK8sOps{
		pods:            make(map[string]*corev1.Pod),
		pvcs:            make(map[string]*corev1.PersistentVolumeClaim),
		configMaps:      make(map[string]*corev1.ConfigMap),
		networkPolicies: make(map[string]*networkingv1.NetworkPolicy),
		execOutput:      make(map[string]string),
		execErr:         make(map[string]error),
	}
}

func (f *fakeK8sOps) createConfigMap(_ context.Context, configMap *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "createConfigMap", pod: configMap.Name})
	if f.configMapCreateErr != nil {
		return nil, f.configMapCreateErr
	}
	if _, exists := f.configMaps[configMap.Name]; exists {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, configMap.Name)
	}
	created := configMap.DeepCopy()
	created.UID = types.UID("fake-configmap-uid-" + created.Name)
	created.ResourceVersion = "1"
	f.configMaps[created.Name] = created
	return created.DeepCopy(), nil
}

func (f *fakeK8sOps) getConfigMap(_ context.Context, name string) (*corev1.ConfigMap, error) {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "getConfigMap", pod: name})
	if f.configMapGetErr != nil {
		return nil, f.configMapGetErr
	}
	configMap, ok := f.configMaps[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return configMap.DeepCopy(), nil
}

func (f *fakeK8sOps) deleteConfigMap(_ context.Context, name string, uid types.UID) error {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "deleteConfigMap", pod: name, uid: uid})
	if f.configMapDeleteErr != nil {
		return f.configMapDeleteErr
	}
	configMap, ok := f.configMaps[name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	if configMap.UID != uid {
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, name, fmt.Errorf("UID precondition failed: expected %q, got %q", uid, configMap.UID))
	}
	delete(f.configMaps, name)
	return nil
}

func (f *fakeK8sOps) createNetworkPolicy(_ context.Context, policy *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error) {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "createNetworkPolicy", pod: policy.Name})
	if f.networkCreateErr != nil {
		return nil, f.networkCreateErr
	}
	if _, exists := f.networkPolicies[policy.Name]; exists {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "networkpolicies"}, policy.Name)
	}
	created := policy.DeepCopy()
	created.UID = types.UID("fake-network-policy-uid-" + policy.Name)
	created.ResourceVersion = "1"
	f.networkPolicies[policy.Name] = created
	return created.DeepCopy(), nil
}

func (f *fakeK8sOps) getNetworkPolicy(_ context.Context, name string) (*networkingv1.NetworkPolicy, error) {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "getNetworkPolicy", pod: name})
	if f.networkGetErr != nil {
		return nil, f.networkGetErr
	}
	policy, ok := f.networkPolicies[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "networkpolicies"}, name)
	}
	return policy.DeepCopy(), nil
}

func (f *fakeK8sOps) listNetworkPolicies(_ context.Context, selector string) ([]networkingv1.NetworkPolicy, error) {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "listNetworkPolicies", selector: selector})
	if f.networkListErr != nil {
		return nil, f.networkListErr
	}
	var result []networkingv1.NetworkPolicy
	for _, policy := range f.networkPolicies {
		if matchesLabels(policy.Labels, selector) {
			result = append(result, *policy.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sOps) deleteNetworkPolicy(_ context.Context, name string, uid types.UID) error {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "deleteNetworkPolicy", pod: name, uid: uid})
	if f.networkDeleteErr != nil {
		return f.networkDeleteErr
	}
	policy, ok := f.networkPolicies[name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "networkpolicies"}, name)
	}
	if uid == "" || policy.UID != uid {
		return apierrors.NewConflict(schema.GroupResource{Resource: "networkpolicies"}, name, fmt.Errorf("UID precondition failed"))
	}
	delete(f.networkPolicies, name)
	return nil
}

func (f *fakeK8sOps) createPVC(_ context.Context, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "createPVC", pod: pvc.Name})
	if f.pvcCreateErr != nil {
		return nil, f.pvcCreateErr
	}
	if _, exists := f.pvcs[pvc.Name]; exists {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, pvc.Name)
	}
	created := pvc.DeepCopy()
	if created.UID == "" {
		created.UID = types.UID("fake-pvc-uid-" + pvc.Name)
	}
	created.ResourceVersion = "1"
	f.pvcs[pvc.Name] = created
	return created.DeepCopy(), nil
}

func (f *fakeK8sOps) getPVC(_ context.Context, name string) (*corev1.PersistentVolumeClaim, error) {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "getPVC", pod: name})
	if f.pvcGetErr != nil {
		return nil, f.pvcGetErr
	}
	pvc, ok := f.pvcs[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, name)
	}
	return pvc.DeepCopy(), nil
}

func (f *fakeK8sOps) listPVCs(_ context.Context, selector string) ([]corev1.PersistentVolumeClaim, error) {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "listPVCs", selector: selector})
	if f.pvcListErr != nil {
		return nil, f.pvcListErr
	}
	var result []corev1.PersistentVolumeClaim
	for _, pvc := range f.pvcs {
		if matchesLabels(pvc.Labels, selector) {
			result = append(result, *pvc.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sOps) deletePVC(_ context.Context, name string, uid types.UID) error {
	f.pvcMu.Lock()
	defer f.pvcMu.Unlock()
	f.calls = append(f.calls, fakeCall{method: "deletePVC", pod: name, uid: uid})
	if f.pvcDeleteErr != nil {
		return f.pvcDeleteErr
	}
	pvc, ok := f.pvcs[name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, name)
	}
	if uid == "" || pvc.UID != uid {
		return apierrors.NewConflict(schema.GroupResource{Resource: "persistentvolumeclaims"}, name, fmt.Errorf("UID precondition failed"))
	}
	delete(f.pvcs, name)
	return nil
}

func (f *fakeK8sOps) record(method, pod string, cmd []string) {
	f.calls = append(f.calls, fakeCall{method: method, pod: pod, cmd: cmd})
}

func (f *fakeK8sOps) createPod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	f.record("createPod", pod.Name, nil)
	if f.createErr != nil {
		return nil, f.createErr
	}
	p := pod.DeepCopy()
	if p.UID == "" {
		p.UID = types.UID("fake-uid-" + p.Name)
	}
	p.Status.Phase = corev1.PodRunning
	f.pods[pod.Name] = p
	return p, nil
}

func (f *fakeK8sOps) getPod(_ context.Context, name string) (*corev1.Pod, error) {
	f.record("getPod", name, nil)
	if f.getErr != nil {
		return nil, f.getErr
	}
	p, ok := f.pods[name]
	if !ok {
		return nil, fmt.Errorf("pod %q not found", name)
	}
	return p.DeepCopy(), nil
}

func (f *fakeK8sOps) deletePod(_ context.Context, name string, uid types.UID, _ int64) error {
	f.calls = append(f.calls, fakeCall{method: "deletePod", pod: name, uid: uid})
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if uid == "" {
		return fmt.Errorf("refusing to delete pod %q without an immutable UID", name)
	}
	if f.beforeDelete != nil {
		f.beforeDelete(name)
	}
	if current, ok := f.pods[name]; ok && current.UID != uid {
		return apierrors.NewConflict(
			schema.GroupResource{Group: "", Resource: "pods"},
			name,
			fmt.Errorf("UID precondition failed: expected %q, got %q", uid, current.UID),
		)
	}
	delete(f.pods, name)
	return nil
}

func (f *fakeK8sOps) listPods(_ context.Context, selector string, fieldSelector string) ([]corev1.Pod, error) {
	f.calls = append(f.calls, fakeCall{method: "listPods", selector: selector})
	if f.listErr != nil {
		return nil, f.listErr
	}

	// Parse label selector to filter pods.
	var result []corev1.Pod
	for _, p := range f.pods {
		if matchesSelector(p, selector) && matchesFieldSelector(p, fieldSelector) {
			result = append(result, *p.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sOps) execInPod(_ context.Context, pod, container string, cmd []string, _ io.Reader) (string, error) {
	f.calls = append(f.calls, fakeCall{method: "execInPod", pod: pod, container: container, cmd: cmd})
	if f.execFunc != nil {
		return f.execFunc(pod, cmd)
	}
	key := execKey(pod, cmd)
	if err, ok := f.execErr[key]; ok {
		return "", err
	}
	if out, ok := f.execOutput[key]; ok {
		return out, nil
	}
	return "", nil
}

// setExecResult configures the fake to return specific output for a pod+cmd combo.
// Clears any conflicting entry in the other map.
func (f *fakeK8sOps) setExecResult(pod string, cmd []string, output string, err error) { //nolint:unparam // pod varies by caller context
	key := execKey(pod, cmd)
	if err != nil {
		f.execErr[key] = err
		delete(f.execOutput, key)
	} else {
		f.execOutput[key] = output
		delete(f.execErr, key)
	}
}

func execKey(pod string, cmd []string) string {
	return pod + ":" + strings.Join(cmd, " ")
}

// matchesSelector does simple label matching for the fake.
func matchesSelector(p *corev1.Pod, selector string) bool {
	return matchesLabels(p.Labels, selector)
}

func matchesLabels(labels map[string]string, selector string) bool {
	if selector == "" {
		return true
	}
	for _, part := range strings.Split(selector, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if labels[kv[0]] != kv[1] {
			return false
		}
	}
	return true
}

// matchesFieldSelector does simple field matching for the fake.
func matchesFieldSelector(p *corev1.Pod, fieldSelector string) bool {
	if fieldSelector == "" {
		return true
	}
	for _, part := range strings.Split(fieldSelector, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] == "status.phase" {
			if string(p.Status.Phase) != kv[1] {
				return false
			}
		}
	}
	return true
}
