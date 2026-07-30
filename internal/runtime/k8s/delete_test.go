package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestRealK8sOpsDeletePodUsesUIDPrecondition(t *testing.T) {
	client := fake.NewSimpleClientset()
	var got metav1.DeleteOptions
	client.Fake.PrependReactor(
		"delete",
		"pods",
		func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			deleteAction, ok := action.(k8stesting.DeleteAction)
			if !ok {
				t.Fatalf("action type = %T, want DeleteAction", action)
			}
			got = deleteAction.GetDeleteOptions()
			return true, nil, nil
		},
	)
	ops := &realK8sOps{clientset: client, namespace: "workers"}
	uid := types.UID("pod-uid-123")

	if err := ops.deletePod(context.Background(), "worker", uid, 5); err != nil {
		t.Fatalf("deletePod: %v", err)
	}
	if got.Preconditions == nil || got.Preconditions.UID == nil {
		t.Fatal("DELETE omitted metadata.preconditions.uid")
	}
	if *got.Preconditions.UID != uid {
		t.Fatalf("DELETE UID = %q, want %q", *got.Preconditions.UID, uid)
	}
	if got.GracePeriodSeconds == nil || *got.GracePeriodSeconds != 5 {
		t.Fatalf("DELETE grace period = %v, want 5", got.GracePeriodSeconds)
	}
}

func TestRealK8sOpsDeletePodRejectsMissingUID(t *testing.T) {
	client := fake.NewSimpleClientset()
	ops := &realK8sOps{clientset: client, namespace: "workers"}

	if err := ops.deletePod(context.Background(), "worker", "", 5); err == nil {
		t.Fatal("deletePod must reject an empty UID")
	}
	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("empty-UID delete reached the API: %#v", actions)
	}
}
