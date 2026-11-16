// Package kube is the changes kheck makes in Kubernetes, and what it reads to
// decide whether to.
package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// ControlPlaneLabel marks control-plane nodes.
const ControlPlaneLabel = "node-role.kubernetes.io/control-plane"

// Cluster wraps a Kubernetes client.
type Cluster struct {
	K   kubernetes.Interface
	Now func() time.Time
}

// New returns a Cluster.
func New(k kubernetes.Interface) *Cluster { return &Cluster{K: k, Now: time.Now} }

// Restart rolls a workload's pods, as kubectl rollout restart does.
func (c *Cluster) Restart(ctx context.Context, ns, kind, name string) error {
	patch := fmt.Appendf(nil, `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, c.Now().Format(time.RFC3339))
	var err error
	switch kind {
	case "deployment":
		_, err = c.K.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case "statefulset":
		_, err = c.K.AppsV1().StatefulSets(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case "daemonset":
		_, err = c.K.AppsV1().DaemonSets(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	default:
		err = errors.New("kind must be deployment, statefulset or daemonset")
	}
	return err
}

// DeletePod deletes a pod so its controller makes a new one.
func (c *Cluster) DeletePod(ctx context.Context, ns, name string) error {
	return c.K.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

// Scale sets a deployment's or statefulset's replicas.
func (c *Cluster) Scale(ctx context.Context, ns, kind, name string, replicas int32) error {
	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
	var err error
	switch kind {
	case "deployment":
		_, err = c.K.AppsV1().Deployments(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "statefulset":
		_, err = c.K.AppsV1().StatefulSets(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	default:
		err = errors.New("kind must be deployment or statefulset")
	}
	return err
}

// Cordon marks a node (un)schedulable. It doesn't evict anything.
func (c *Cluster) Cordon(ctx context.Context, node string, cordon bool) error {
	patch := fmt.Appendf(nil, `{"spec":{"unschedulable":%v}}`, cordon)
	_, err := c.K.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// Protected says why something mustn't be changed, or "" if it may be: its
// namespace is protected, or it or its namespace has label=true. kind is
// deployment, statefulset, daemonset, pod or node; ns is empty for a node.
func (c *Cluster) Protected(ctx context.Context, namespaces func(string) bool, label, ns, kind, name string) (string, error) {
	if ns != "" {
		if namespaces(ns) {
			return "namespace " + ns + " is protected", nil
		}
		n, err := c.K.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if n.Labels[label] == "true" {
			return "namespace " + ns + " has " + label + "=true", nil
		}
	}
	labels, err := c.labels(ctx, ns, kind, name)
	if err != nil {
		return "", err
	}
	if labels[label] == "true" {
		return kind + " " + name + " has " + label + "=true", nil
	}
	if kind == "pod" {
		// A pod is as protected as what owns it.
		if k, owner := c.Owner(ctx, ns, name); owner != "" {
			if labels, err := c.labels(ctx, ns, k, owner); err == nil && labels[label] == "true" {
				return k + " " + owner + " has " + label + "=true", nil
			}
		}
	}
	return "", nil
}

func (c *Cluster) labels(ctx context.Context, ns, kind, name string) (map[string]string, error) {
	var m metav1.Object
	var err error
	switch kind {
	case "deployment":
		m, err = c.K.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	case "statefulset":
		m, err = c.K.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
	case "daemonset":
		m, err = c.K.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
	case "pod":
		m, err = c.K.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	case "node":
		m, err = c.K.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	default:
		return nil, fmt.Errorf("no kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	return m.GetLabels(), nil
}

// Node is what the gates know about a node.
type Node struct {
	Name                        string
	ControlPlane, Ready, Asleep bool
	Cordoned                    bool
}

// Nodes are every node, with asleep ones marked by asleepTaints.
func (c *Cluster) Nodes(ctx context.Context, asleepTaints []string) ([]Node, error) {
	list, err := c.K.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(list.Items))
	for _, n := range list.Items {
		_, cp := n.Labels[ControlPlaneLabel]
		node := Node{Name: n.Name, ControlPlane: cp, Cordoned: n.Spec.Unschedulable}
		for _, cd := range n.Status.Conditions {
			if cd.Type == corev1.NodeReady {
				node.Ready = cd.Status == corev1.ConditionTrue
			}
		}
		node.Asleep = slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return slices.Contains(asleepTaints, t.Key) })
		out = append(out, node)
	}
	return out, nil
}

// Hopeless says why replacing a workload's pods (or one pod) can't help
// them: their image or config is wrong, they're killed for memory by their
// own limit, they crash for the wrong CPU architecture, the kubelet already
// restarts them for a failing liveness probe, or they can't be scheduled. A
// new pod would have the same problem. It's empty when a fresh pod may help,
// e.g. a wedged process.
func (c *Cluster) Hopeless(ctx context.Context, ns, kind, name string) (string, error) {
	pods, err := c.pods(ctx, ns, kind, name)
	if err != nil {
		return "", err
	}
	var why []string
	for i := range pods {
		w := hopeless(&pods[i])
		if w == "" {
			w = c.crashedOn(ctx, &pods[i])
		}
		if w == "" {
			w = c.probeKilled(ctx, &pods[i])
		}
		if w == "" && !ready(&pods[i]) {
			return "", nil // a pod that's failing for a reason a fresh one may not have
		}
		if w != "" {
			why = append(why, pods[i].Name+": "+w)
		}
	}
	return strings.Join(why, "; "), nil
}

func (c *Cluster) pods(ctx context.Context, ns, kind, name string) ([]corev1.Pod, error) {
	if kind == "pod" {
		p, err := c.K.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return []corev1.Pod{*p}, nil
	}
	var sel *metav1.LabelSelector
	switch kind {
	case "deployment":
		d, err := c.K.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		sel = d.Spec.Selector
	case "statefulset":
		s, err := c.K.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		sel = s.Spec.Selector
	case "daemonset":
		d, err := c.K.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		sel = d.Spec.Selector
	default:
		return nil, errors.New("kind must be deployment, statefulset or daemonset")
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil, err
	}
	list, err := c.K.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: s.String()})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

var configReasons = []string{"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull", "CreateContainerConfigError", "CreateContainerError"}

func hopeless(p *corev1.Pod) string {
	for _, cd := range p.Status.Conditions {
		if cd.Type == corev1.PodScheduled && cd.Status == corev1.ConditionFalse {
			return "it can't be scheduled (" + clip(cd.Message, 160) + "), and a new pod can't either"
		}
	}
	for _, s := range p.Status.ContainerStatuses {
		if w := s.State.Waiting; w != nil && slices.Contains(configReasons, w.Reason) {
			return fmt.Sprintf("%s is %s (%s): its image or config is wrong, and a new pod gets the same", s.Name, w.Reason, clip(w.Message, 160))
		}
		if t := s.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
			return s.Name + " is OOMKilled by its own memory limit, which a new pod has too"
		}
		if t := s.LastTerminationState.Terminated; t != nil && strings.Contains(t.Message, "exec format error") {
			return s.Name + "'s image is built for another CPU architecture"
		}
	}
	return ""
}

// crashedOn says why a crashing container can't be fixed by a new pod, from
// the last lines it wrote before it died.
func (c *Cluster) crashedOn(ctx context.Context, p *corev1.Pod) string {
	for _, s := range p.Status.ContainerStatuses {
		if s.RestartCount == 0 {
			continue
		}
		lines := int64(10)
		rc, err := c.K.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: s.Name, Previous: true, TailLines: &lines}).Stream(ctx)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(rc, 64<<10))
		_ = rc.Close()
		if strings.Contains(string(b), "exec format error") {
			return s.Name + "'s image is built for another CPU architecture (exec format error)"
		}
	}
	return ""
}

// probeKilled says whether the kubelet keeps restarting a container for
// failing its liveness probe: it has already done what a restart would, and a
// new pod gets the same probe.
func (c *Cluster) probeKilled(ctx context.Context, p *corev1.Pod) string {
	var restarts int32
	for _, s := range p.Status.ContainerStatuses {
		restarts = max(restarts, s.RestartCount)
	}
	if restarts < 3 {
		return ""
	}
	evs, err := c.K.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + p.Name})
	if err != nil {
		return ""
	}
	since := c.Now().Add(-time.Hour)
	for _, e := range evs.Items {
		if e.InvolvedObject.Name == p.Name && e.Reason == "Unhealthy" && strings.HasPrefix(e.Message, "Liveness probe failed") && !lastSeen(&e).Before(since) {
			return fmt.Sprintf("the kubelet has restarted it %d times for failing its liveness probe (%s), and a new pod gets the same probe", restarts, clip(e.Message, 160))
		}
	}
	return ""
}

func ready(p *corev1.Pod) bool {
	for _, cd := range p.Status.Conditions {
		if cd.Type == corev1.PodReady {
			return cd.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Settle waits up to timeout for a deployment, statefulset or daemonset to
// have every replica updated and ready, and says how it went.
func (c *Cluster) Settle(ctx context.Context, ns, kind, name string, timeout time.Duration) (string, bool) {
	start := c.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var want, ready, updated int32
	for {
		switch kind {
		case "deployment":
			if d, err := c.K.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
				want, ready, updated = ptr(d.Spec.Replicas, 1), d.Status.ReadyReplicas, d.Status.UpdatedReplicas
			}
		case "statefulset":
			if s, err := c.K.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
				want, ready, updated = ptr(s.Spec.Replicas, 1), s.Status.ReadyReplicas, s.Status.UpdatedReplicas
			}
		case "daemonset":
			if d, err := c.K.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
				want, ready, updated = d.Status.DesiredNumberScheduled, d.Status.NumberReady, d.Status.UpdatedNumberScheduled
			}
		default:
			return "", false
		}
		if want > 0 && ready >= want && updated >= want {
			return fmt.Sprintf("%s %s/%s has %d/%d ready after %s.", kind, ns, name, ready, want, c.Now().Sub(start).Round(time.Second)), true
		}
		select {
		case <-ctx.Done():
			return fmt.Sprintf("%s %s/%s has %d/%d ready after %s: not settled yet; look at its pods.", kind, ns, name, ready, want, timeout), false
		case <-time.After(3 * time.Second):
		}
	}
}

// Owner is a pod's workload, as kind and name, when it's a deployment,
// statefulset or daemonset.
func (c *Cluster) Owner(ctx context.Context, ns, pod string) (string, string) {
	p, err := c.K.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", ""
	}
	kind, name, _ := strings.Cut(Workload(p), "/")
	switch kind {
	case "deployment", "statefulset", "daemonset":
		return kind, name
	}
	return "", ""
}

// Controlled reports whether a pod has a controller to replace it.
func (c *Cluster) Controlled(ctx context.Context, ns, pod string) (bool, error) {
	p, err := c.K.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return len(p.OwnerReferences) > 0, nil
}

// Replicas is how many replicas a deployment or statefulset wants.
func (c *Cluster) Replicas(ctx context.Context, ns, kind, name string) (int32, error) {
	switch kind {
	case "deployment":
		d, err := c.K.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		return ptr(d.Spec.Replicas, 1), nil
	case "statefulset":
		s, err := c.K.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		return ptr(s.Spec.Replicas, 1), nil
	}
	return 0, errors.New("kind must be deployment or statefulset")
}

// Workload names what owns a pod, e.g. deployment/gridtie.
func Workload(p *corev1.Pod) string {
	for _, o := range p.OwnerReferences {
		switch o.Kind {
		case "ReplicaSet":
			if i := strings.LastIndex(o.Name, "-"); i > 0 {
				return "deployment/" + o.Name[:i]
			}
			return "replicaset/" + o.Name
		case "Job":
			if i := strings.LastIndex(o.Name, "-"); i > 0 {
				return "cronjob/" + o.Name[:i]
			}
			return "job/" + o.Name
		default:
			return strings.ToLower(o.Kind) + "/" + o.Name
		}
	}
	return "pod/" + p.Name
}

func lastSeen(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case e.Series != nil:
		return e.Series.LastObservedTime.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func ptr(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Who checks a token with the API server, and returns its user, or "" when
// the token isn't valid for one of the audiences.
func (c *Cluster) Who(ctx context.Context, token string, audiences []string) (string, error) {
	r, err := c.K.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	if !r.Status.Authenticated {
		return "", nil
	}
	return r.Status.User.Username, nil
}
