package kube

import (
	"context"
	"strings"
	"testing"
	"time"

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
	ctx := context.Background()
	pod := func(name string, st corev1.ContainerStatus, ready bool) *corev1.Pod {
		r := corev1.ConditionFalse
		if ready {
			r = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "a", Labels: map[string]string{"app": name[:3]}},
			Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: r}}, ContainerStatuses: []corev1.ContainerStatus{st}},
		}
	}
	dep := func(app string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: "a"}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}}}}
	}
	c := &Cluster{K: fake.NewClientset(
		dep("tag"), pod("tag-1", corev1.ContainerStatus{Name: "c", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling"}}}, false),
		dep("oom"), pod("oom-1", corev1.ContainerStatus{Name: "c", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}, false),
		dep("wed"), pod("wed-1", corev1.ContainerStatus{Name: "c", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, false),
		pod("wed-2", corev1.ContainerStatus{Name: "c", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, true),
		dep("prb"), pod("prb-1", corev1.ContainerStatus{Name: "c", RestartCount: 6, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, false),
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "a"}, Type: "Warning", Reason: "Unhealthy", LastTimestamp: metav1.Now(),
			Message: "Liveness probe failed: connection refused", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "a", Name: "prb-1"},
		},
	), Now: time.Now}
	for name, want := range map[string]string{"tag": "c is ImagePullBackOff", "oom": "OOMKilled by its own memory limit", "wed": "", "prb": "restarted it 6 times for failing its liveness probe"} {
		why, err := c.Hopeless(ctx, "a", "deployment", name)
		if err != nil || want == "" && why != "" || want != "" && !strings.Contains(why, want) {
			t.Errorf("%s: %q, %v", name, why, err)
		}
	}
	if why, _ := c.Hopeless(ctx, "a", "pod", "wed-1"); why != "" {
		t.Errorf("a wedged pod is hopeless: %q", why)
	}
}

func TestSettle(t *testing.T) {
	two := int32(2)
	c := &Cluster{K: fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "a"}, Spec: appsv1.DeploymentSpec{Replicas: &two},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, UpdatedReplicas: 2},
	}), Now: time.Now}
	if got, ok := c.Settle(context.Background(), "a", "deployment", "api", time.Second); !ok || !strings.HasPrefix(got, "deployment a/api has 2/2 ready after") {
		t.Errorf("settle = %q", got)
	}
	c.K = fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "a"}, Spec: appsv1.DeploymentSpec{Replicas: &two},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 2},
	})
	if got, ok := c.Settle(context.Background(), "a", "deployment", "api", 100*time.Millisecond); ok || !strings.Contains(got, "1/2 ready") || !strings.Contains(got, "not settled yet") {
		t.Errorf("unsettled = %q", got)
	}
}
