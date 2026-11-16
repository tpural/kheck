// Package config is kheck's configuration: who may ask for changes, what may
// never be changed, and the gates a change must pass.
package config

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	"sigs.k8s.io/yaml"
)

// Actions are the changes kheck can make.
var Actions = []string{"restart", "delete_pod", "scale", "cordon", "uncordon"}

// Config is kheck's configuration.
type Config struct {
	Listen string `json:"listen"`
	// Callers are the Kubernetes users (names or globs, e.g.
	// system:serviceaccount:agents:*) that may ask for changes.
	Callers []string `json:"callers"`
	// Audiences a caller's token must be for. A token made for kheck can't
	// be used against the API server, and kheck can't replay it there.
	Audiences []string `json:"audiences"`
	Metrics   Metrics  `json:"metrics,omitzero"`
	// Settle is how long a change waits for its workload to be ready again.
	Settle  Duration `json:"settle"`
	Protect Protect  `json:"protect"`
	// AsleepTaints mark nodes that are down on purpose: they don't count as
	// not ready or cordoned.
	AsleepTaints []string `json:"asleepTaints"`
	Gates        []Gate   `json:"gates"`
}

// Metrics is a Prometheus-compatible API, for query gates.
type Metrics struct {
	URL string `json:"url"`
}

// Protect is what's never changed, whatever the gates say.
type Protect struct {
	// Namespaces are names or globs, e.g. kube-system or cert-*.
	Namespaces []string `json:"namespaces"`
	// Label set to "true" on a namespace, workload, pod or node protects it.
	Label string `json:"label"`
}

// Gate is a rule some changes must pass. All of Require must hold.
type Gate struct {
	Name string `json:"name"`
	// Actions it applies to; empty is all.
	Actions []string `json:"actions"`
	// Namespaces (names or globs) it applies to; empty is all. A gate with
	// namespaces doesn't apply to nodes.
	Namespaces []string `json:"namespaces"`
	Require    Require  `json:"require"`
}

// Require are a gate's conditions; one left out isn't checked.
type Require struct {
	// ControlPlane is whether the node may be a control-plane node.
	ControlPlane *bool `json:"controlPlane,omitempty"`
	// OtherNodesReady: every other node is Ready, except asleep ones.
	OtherNodesReady *bool `json:"otherNodesReady,omitempty"`
	// MaxCordoned is how many other nodes may already be cordoned, not
	// counting asleep ones.
	MaxCordoned *int `json:"maxCordoned,omitempty"`
	// MinReplicas and MaxReplicas bound what scale may set.
	MinReplicas *int `json:"minReplicas,omitempty"`
	MaxReplicas *int `json:"maxReplicas,omitempty"`
	// Query is PromQL, run against metrics.url, that must return
	// something, and nothing zero. $NAMESPACE, $NAME and $NODE are
	// replaced with the change's target.
	Query string `json:"query,omitempty"`
}

// Duration is a time.Duration written like "90s".
type Duration struct{ time.Duration }

// UnmarshalJSON parses "90s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Parse reads a config, filling in defaults.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Settle.Duration == 0 {
		c.Settle.Duration = 90 * time.Second
	}
	if c.Protect.Label == "" {
		c.Protect.Label = "kheck/protected"
	}
	if len(c.Audiences) == 0 {
		c.Audiences = []string{"kheck"}
	}
	if len(c.Callers) == 0 {
		return nil, errors.New("callers is required: the Kubernetes users that may ask for changes")
	}
	for _, p := range c.Callers {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("callers: %q: %w", p, err)
		}
	}
	for _, p := range c.Protect.Namespaces {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("protect.namespaces: %q: %w", p, err)
		}
	}
	for i, g := range c.Gates {
		if g.Name == "" {
			return nil, fmt.Errorf("gates[%d]: a gate needs a name, to say which refused", i)
		}
		for _, a := range g.Actions {
			if !slices.Contains(Actions, a) {
				return nil, fmt.Errorf("gate %q: no action %q; there's %v", g.Name, a, Actions)
			}
		}
		for _, p := range g.Namespaces {
			if _, err := path.Match(p, ""); err != nil {
				return nil, fmt.Errorf("gate %q: namespaces: %q: %w", g.Name, p, err)
			}
		}
		if g.Require.Query != "" && c.Metrics.URL == "" {
			return nil, fmt.Errorf("gate %q: a query needs metrics.url", g.Name)
		}
		if r := g.Require; r.MinReplicas != nil && r.MaxReplicas != nil && *r.MinReplicas > *r.MaxReplicas {
			return nil, fmt.Errorf("gate %q: minReplicas is more than maxReplicas", g.Name)
		}
	}
	return &c, nil
}

// Matches reports whether name matches one of the globs.
func Matches(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}
