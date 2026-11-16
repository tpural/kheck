package gate

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tpural/kheck/internal/config"
	"github.com/tpural/kheck/internal/kube"
)

func node(name string, ready, cordoned bool, labels map[string]string, taints ...string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: corev1.NodeSpec{Unschedulable: cordoned}}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	for _, t := range taints {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: t, Effect: corev1.TaintEffectNoSchedule})
	}
	return n
}

type metrics map[string][]float64

func (m metrics) Query(_ context.Context, q string) ([]float64, error) { return m[q], nil }

func TestCheck(t *testing.T) {
	no, yes, zero := false, true, 0
	one, ten := 1, 10
	cp := map[string]string{kube.ControlPlaneLabel: "true"}
	k := fake.NewClientset(
		node("cp1", true, false, cp),
		node("w1", true, false, nil),
		node("w2", true, false, nil),
		node("gpu", false, true, nil, "example.com/asleep"), // asleep: neither down nor cordoned
	)
	g := &Gates{
		Cluster: kube.New(k), AsleepTaints: []string{"example.com/asleep"},
		Metrics: metrics{`quiet{namespace="ha"}`: {1}, `quiet{namespace="busy"}`: {0}},
		Gates: []config.Gate{
			{Name: "one node at a time", Actions: []string{"cordon"}, Require: config.Require{ControlPlane: &no, OtherNodesReady: &yes, MaxCordoned: &zero}},
			{Name: "replicas", Actions: []string{"scale"}, Require: config.Require{MinReplicas: &one, MaxReplicas: &ten}},
			{Name: "quiet", Actions: []string{"restart"}, Namespaces: []string{"ha", "busy"}, Require: config.Require{Query: `quiet{namespace="$NAMESPACE"}`}},
		},
	}
	ctx := context.Background()
	for _, tc := range []struct {
		c    Change
		want string
	}{
		{Change{Action: "cordon", Kind: "node", Name: "w1"}, ""},
		{Change{Action: "cordon", Kind: "node", Name: "cp1"}, `gate "one node at a time": cp1 is a control-plane node`},
		{Change{Action: "uncordon", Kind: "node", Name: "cp1"}, ""},
		{Change{Action: "scale", Namespace: "a", Kind: "deployment", Name: "x", Replicas: 2}, ""},
		{Change{Action: "scale", Namespace: "a", Kind: "deployment", Name: "x", Replicas: 0}, `gate "replicas": 0 replicas is fewer than 1`},
		{Change{Action: "scale", Namespace: "a", Kind: "deployment", Name: "x", Replicas: 11}, "more than 10"},
		{Change{Action: "restart", Namespace: "ha", Kind: "deployment", Name: "x"}, ""},
		{Change{Action: "restart", Namespace: "busy", Kind: "deployment", Name: "x"}, `gate "quiet": query quiet{namespace="busy"} isn't true`},
		{Change{Action: "restart", Namespace: "other", Kind: "deployment", Name: "x"}, ""},
	} {
		got, err := g.Check(ctx, tc.c)
		if err != nil || !strings.Contains(got, tc.want) || (tc.want == "") != (got == "") {
			t.Errorf("%+v: %q, %v; want %q", tc.c, got, err, tc.want)
		}
	}

	// A query that finds nothing refuses.
	g.Metrics = metrics{}
	if got, _ := g.Check(ctx, Change{Action: "restart", Namespace: "ha", Kind: "deployment", Name: "x"}); !strings.Contains(got, "found nothing") {
		t.Errorf("got %q", got)
	}

	// Another node cordoned, or one down, refuses a cordon.
	n, _ := k.CoreV1().Nodes().Get(ctx, "w2", metav1.GetOptions{})
	n.Spec.Unschedulable = true
	_, _ = k.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
	if got, _ := g.Check(ctx, Change{Action: "cordon", Kind: "node", Name: "w1"}); !strings.Contains(got, "1 other nodes are already cordoned (w2)") {
		t.Errorf("got %q", got)
	}
	n.Spec.Unschedulable = false
	n.Status.Conditions[0].Status = corev1.ConditionFalse
	_, _ = k.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
	if got, _ := g.Check(ctx, Change{Action: "cordon", Kind: "node", Name: "w1"}); !strings.Contains(got, "not every other node is Ready: w2") {
		t.Errorf("got %q", got)
	}
}
