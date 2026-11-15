package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tpural/kheck/internal/config"
	"github.com/tpural/kheck/internal/gate"
	"github.com/tpural/kheck/internal/kube"
	"github.com/tpural/kheck/internal/tremolo"
)

// fakeTremolo knows one token, and refuses actions while refusing is set.
type fakeTremolo struct {
	mu       sync.Mutex
	did      []string
	refusing bool
}

func (f *fakeTremolo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer good" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/api/me":
		_, _ = io.WriteString(w, `{"name":"tp","token":{"name":"upkeeper"}}`)
	case strings.HasSuffix(r.URL.Path, "/actions") && f.refusing:
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"agents are paused"}`)
	case strings.HasSuffix(r.URL.Path, "/actions"):
		var a tremolo.Action
		_ = json.Unmarshal(b, &a)
		f.did = append(f.did, "begin "+a.Action+" "+a.Target+" why "+a.Why)
		_, _ = io.WriteString(w, `{"id":9}`)
	case strings.Contains(r.URL.Path, "/actions/"):
		f.did = append(f.did, "end "+strings.TrimSpace(string(b)))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeTremolo) took() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := strings.Join(f.did, "; ")
	f.did = nil
	return s
}

type bearer struct {
	token string
	rt    http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.rt.RoundTrip(r)
}

func one(n int32) *int32 { return &n }

func TestServer(t *testing.T) {
	ctx := context.Background()
	ft := &fakeTremolo{}
	ts := httptest.NewServer(ft)
	defer ts.Close()
	labels := map[string]string{"app": "api"}
	dep := func(ns, name string, l map[string]string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: l},
			Spec:       appsv1.DeploymentSpec{Replicas: one(1), Selector: &metav1.LabelSelector{MatchLabels: labels}},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 1},
		}
	}
	ns := func(name string, l map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
	}
	k := fake.NewClientset(
		ns("app", nil), ns("kube-system", nil), ns("db", map[string]string{"kheck/protected": "true"}),
		dep("app", "api", nil), dep("app", "billing", map[string]string{"kheck/protected": "true"}),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "lone"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "bad", Labels: map[string]string{"app": "other"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-5d4"}}},
			Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "api", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}},
		},
	)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
tremolo: {url: %s}
settle: 1s
protect: {namespaces: [kube-system]}
gates: [{name: replicas, actions: [scale], require: {minReplicas: 1}}]
`, ts.URL)))
	if err != nil {
		t.Fatal(err)
	}
	cluster := kube.New(k)
	tc := tremolo.New(ts.URL)
	s := &Server{
		Config: cfg, Cluster: cluster, Tremolo: tc, Log: slog.New(slog.DiscardHandler),
		Gates: &gate.Gates{Gates: cfg.Gates, Cluster: cluster, Metrics: tc},
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	connect := func(token string) (*mcp.ClientSession, error) {
		c := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
		return c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}, Timeout: 10 * time.Second}}, nil)
	}
	// Only a token tremolo knows gets in.
	if _, err := connect("bad"); err == nil {
		t.Fatal("a token tremolo doesn't know connected")
	}
	cs, err := connect("good")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args map[string]any) Result {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var r Result
		if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	restart := func(ns, name string) map[string]any {
		return map[string]any{"investigation": 3, "namespace": ns, "kind": "deployment", "name": name, "why": "wedged"}
	}

	// A change that passes is recorded as the caller, made, and waited on.
	r := call("restart", restart("app", "api"))
	if r.Result != "done" || !r.Ready || !strings.Contains(r.Settled, "1/1 ready") {
		t.Errorf("restart %+v", r)
	}
	if got, want := ft.took(), `begin rollout restart deployment/api why wedged; end {"result":"done"}`; got != want {
		t.Errorf("tremolo got %q, want %q", got, want)
	}
	d, _ := k.AppsV1().Deployments("app").Get(ctx, "api", metav1.GetOptions{})
	if d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] == "" {
		t.Error("not restarted")
	}

	// Refusals never reach tremolo, let alone the cluster.
	for _, tc := range []struct {
		tool string
		args map[string]any
		want Result
	}{
		{"restart", restart("kube-system", "coredns"), refused("namespace kube-system is protected")},
		{"restart", restart("db", "postgres"), refused("namespace db has kheck/protected=true")},
		{"restart", restart("app", "billing"), refused("deployment billing has kheck/protected=true")},
		{"scale", map[string]any{"investigation": 3, "namespace": "app", "kind": "deployment", "name": "api", "replicas": 0, "why": "x"}, refused(`gate "replicas": 0 replicas is fewer than 1`)},
		{"scale", map[string]any{"investigation": 3, "namespace": "app", "kind": "deployment", "name": "api", "replicas": 1, "why": "x"}, failed("it already wants 1; scaling isn't a way to restart")},
		{"delete_pod", map[string]any{"investigation": 3, "namespace": "app", "name": "lone", "why": "x"}, failed("this pod has no controller, so it wouldn't come back")},
		{"restart", map[string]any{"investigation": 3, "namespace": "app", "kind": "deployment", "name": "api", "why": " "}, failed("say why: what you found that this should fix")},
		{"restart", map[string]any{"investigation": 3, "namespace": "app", "kind": "cronjob", "name": "api", "why": "x"}, failed("kind must be deployment, statefulset or daemonset")},
		{"restart", map[string]any{"investigation": 3, "namespace": "App!", "kind": "deployment", "name": "api", "why": "x"}, failed("namespace and name must be Kubernetes names")},
	} {
		if r := call(tc.tool, tc.args); r != tc.want {
			t.Errorf("%s %v: %+v, want %+v", tc.tool, tc.args, r, tc.want)
		}
	}
	if got := ft.took(); got != "" {
		t.Errorf("refusals reached tremolo: %s", got)
	}

	// A new pod can't fix a bad image.
	if r := call("delete_pod", map[string]any{"investigation": 3, "namespace": "app", "name": "bad", "why": "x"}); r.Result != "failed" || !strings.Contains(r.Why, "a new pod can't fix this: bad: api is ImagePullBackOff") {
		t.Errorf("delete_pod %+v", r)
	}

	// tremolo refusing (paused, over budget) stops it before the cluster.
	ft.refusing = true
	k.ClearActions()
	if r := call("restart", restart("app", "api")); r.Result != "refused" || !strings.Contains(r.Why, "tremolo: 409: agents are paused") {
		t.Errorf("restart while paused %+v", r)
	}
	for _, a := range k.Actions() {
		if a.GetVerb() == "patch" {
			t.Errorf("changed the cluster while tremolo refused: %v", a)
		}
	}
}
