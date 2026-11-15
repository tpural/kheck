package kube

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestProtected(t *testing.T) {
	ctx := context.Background()
	c := New(fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "db", Labels: map[string]string{"kheck/protected": "true"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "db-7f9-x2", OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "db-7f9"}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"kheck/protected": "true"}}},
	))
	none := func(string) bool { return false }
	for _, tc := range []struct{ ns, kind, name, want string }{
		{"app", "pod", "db-7f9-x2", "deployment db has kheck/protected=true"},
		{"app", "pod", "web-1", ""},
		{"", "node", "n1", "node n1 has kheck/protected=true"},
	} {
		got, err := c.Protected(ctx, none, "kheck/protected", tc.ns, tc.kind, tc.name)
		if err != nil || got != tc.want {
			t.Errorf("%s/%s: %q, %v; want %q", tc.ns, tc.name, got, err, tc.want)
		}
	}
	// What it can't look up isn't assumed unprotected.
	if _, err := c.Protected(ctx, none, "kheck/protected", "gone", "pod", "x"); err == nil {
		t.Error("a missing namespace passed")
	}
}

func TestHopeless(t *testing.T) {
	oom := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "p"}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "app", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
	}}}
	why, err := New(fake.NewClientset(oom)).Hopeless(context.Background(), "app", "pod", "p")
	if err != nil || !strings.Contains(why, "OOMKilled by its own memory limit") {
		t.Errorf("%q, %v", why, err)
	}
}
