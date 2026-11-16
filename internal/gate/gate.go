// Package gate decides whether a change passes the configured gates.
package gate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/tpural/kheck/internal/config"
	"github.com/tpural/kheck/internal/kube"
)

// Change is what a caller asks for.
type Change struct {
	Action string // one of config.Actions
	// Namespace is empty for a node.
	Namespace, Kind, Name string
	// Replicas is what scale sets.
	Replicas int32
}

// Querier runs PromQL.
type Querier interface {
	Query(ctx context.Context, promql string) ([]float64, error)
}

// Gates check changes against the config's gates.
type Gates struct {
	Gates        []config.Gate
	Cluster      *kube.Cluster
	Metrics      Querier
	AsleepTaints []string
}

// Check says which gate refuses a change and why, or "" when all pass. An
// error is a gate it couldn't check, which refuses too.
func (g *Gates) Check(ctx context.Context, c Change) (string, error) {
	var nodes []kube.Node
	for _, gt := range g.Gates {
		if !applies(gt, c) {
			continue
		}
		r := gt.Require
		if (r.OtherNodesReady != nil || r.MaxCordoned != nil || r.ControlPlane != nil) && nodes == nil {
			var err error
			if nodes, err = g.Cluster.Nodes(ctx, g.AsleepTaints); err != nil {
				return "", fmt.Errorf("gate %q: %w", gt.Name, err)
			}
		}
		why, err := g.check(ctx, r, c, nodes)
		if err != nil {
			return "", fmt.Errorf("gate %q: %w", gt.Name, err)
		}
		if why != "" {
			return fmt.Sprintf("gate %q: %s", gt.Name, why), nil
		}
	}
	return "", nil
}

func applies(g config.Gate, c Change) bool {
	if len(g.Actions) > 0 && !slices.Contains(g.Actions, c.Action) {
		return false
	}
	if len(g.Namespaces) > 0 {
		return c.Namespace != "" && config.Matches(g.Namespaces, c.Namespace)
	}
	return true
}

func (g *Gates) check(ctx context.Context, r config.Require, c Change, nodes []kube.Node) (string, error) {
	node := c.Kind == "node"
	if r.ControlPlane != nil && node {
		i := slices.IndexFunc(nodes, func(n kube.Node) bool { return n.Name == c.Name })
		if i < 0 {
			return "", fmt.Errorf("no node %s", c.Name)
		}
		if nodes[i].ControlPlane && !*r.ControlPlane {
			return c.Name + " is a control-plane node", nil
		}
	}
	if r.OtherNodesReady != nil && *r.OtherNodesReady {
		var down []string
		for _, n := range nodes {
			if n.Name != c.Name && !n.Ready && !n.Asleep {
				down = append(down, n.Name)
			}
		}
		if len(down) > 0 {
			return "not every other node is Ready: " + strings.Join(down, ", "), nil
		}
	}
	if r.MaxCordoned != nil {
		var cordoned []string
		for _, n := range nodes {
			if n.Name != c.Name && n.Cordoned && !n.Asleep {
				cordoned = append(cordoned, n.Name)
			}
		}
		if len(cordoned) > *r.MaxCordoned {
			return fmt.Sprintf("%d other nodes are already cordoned (%s); at most %d may be", len(cordoned), strings.Join(cordoned, ", "), *r.MaxCordoned), nil
		}
	}
	if c.Action == "scale" {
		if r.MinReplicas != nil && int(c.Replicas) < *r.MinReplicas {
			return fmt.Sprintf("%d replicas is fewer than %d", c.Replicas, *r.MinReplicas), nil
		}
		if r.MaxReplicas != nil && int(c.Replicas) > *r.MaxReplicas {
			return fmt.Sprintf("%d replicas is more than %d", c.Replicas, *r.MaxReplicas), nil
		}
	}
	if r.Query != "" {
		q := r.Query
		if node {
			q = strings.ReplaceAll(q, "$NODE", c.Name)
		} else {
			q = strings.NewReplacer("$NAMESPACE", c.Namespace, "$NAME", c.Name).Replace(q)
		}
		vs, err := g.Metrics.Query(ctx, q)
		if err != nil {
			return "", fmt.Errorf("query %s: %w", q, err)
		}
		if len(vs) == 0 {
			return "query " + q + " found nothing", nil
		}
		for _, v := range vs {
			if v == 0 {
				return "query " + q + " isn't true", nil
			}
		}
	}
	return "", nil
}
