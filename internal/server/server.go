// Package server is kheck's MCP server: the changes agents may ask for, each
// checked, made, logged, and waited on.
package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tpural/kheck/internal/config"
	"github.com/tpural/kheck/internal/gate"
	"github.com/tpural/kheck/internal/kube"
)

// Server serves kheck's tools.
type Server struct {
	Config  *config.Config
	Cluster *kube.Cluster
	Gates   *gate.Gates
	Log     *slog.Logger
	Version string

	mu     sync.Mutex
	tokens map[[32]byte]known
}

type known struct {
	name string
	at   time.Time
}

// tokenFor is how long a checked token is trusted before checking it again.
const tokenFor = time.Minute

// Handler serves MCP at /mcp and a health check at /healthz.
func (s *Server) Handler() http.Handler {
	ms := mcp.NewServer(&mcp.Implementation{Name: "kheck", Version: s.Version}, nil)
	s.tools(ms)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ms }, &mcp.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", auth.RequireBearerToken(s.verify, nil)(h))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

// verify checks a token with the API server: it must be for one of the
// audiences, and its user one of the callers.
func (s *Server) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	k, ok := s.tokens[key]
	s.mu.Unlock()
	if !ok || time.Since(k.at) > tokenFor {
		name, err := s.Cluster.Who(ctx, token, s.Config.Audiences)
		if err != nil {
			return nil, fmt.Errorf("checking the token: %w", err)
		}
		if name == "" {
			return nil, auth.ErrInvalidToken
		}
		if !config.Matches(s.Config.Callers, name) {
			s.Log.Warn("not a caller", "user", name)
			return nil, auth.ErrInvalidToken
		}
		k = known{name, time.Now()}
		s.mu.Lock()
		if s.tokens == nil {
			s.tokens = map[[32]byte]known{}
		}
		for key, old := range s.tokens {
			if time.Since(old.at) > tokenFor {
				delete(s.tokens, key)
			}
		}
		s.tokens[key] = k
		s.mu.Unlock()
	}
	return &auth.TokenInfo{UserID: k.name, Expiration: k.at.Add(tokenFor)}, nil
}

// Result is how a change went.
type Result struct {
	// Result is done, refused (a rule said no: don't work around it) or
	// failed (it couldn't be done as asked).
	Result  string `json:"result"`
	Why     string `json:"why,omitempty"`
	Settled string `json:"settled,omitempty"`
	// Ready is the workload coming back ready after the change.
	Ready bool `json:"ready,omitempty"`
}

func refused(why string) Result { return Result{Result: "refused", Why: why} }
func failed(why string) Result  { return Result{Result: "failed", Why: why} }

func callerOf(req *mcp.CallToolRequest) string {
	if req.Extra == nil || req.Extra.TokenInfo == nil {
		return ""
	}
	return req.Extra.TokenInfo.UserID
}

var kname = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// change is one request to change something, and how to make it.
type change struct {
	gate.Change
	ref string
	why string
	// pre refuses what can't work, e.g. a new pod for a bad image.
	pre    func(context.Context) (string, error)
	do     func(context.Context) error
	settle func(context.Context) (string, bool)
}

// act checks a change, makes it, logs it and waits for it to settle.
// Anything it can't check refuses it.
func (s *Server) act(ctx context.Context, who string, c change) Result {
	r := s.act1(ctx, who, c)
	s.Log.Info("change", "by", who, "ref", c.ref, "action", c.Action, "namespace", c.Namespace, "kind", c.Kind, "name", c.Name,
		"why", c.why, "result", r.Result, "detail", r.Why)
	return r
}

func (s *Server) act1(ctx context.Context, who string, c change) Result {
	switch {
	case who == "":
		return refused("no caller")
	case strings.TrimSpace(c.why) == "":
		return failed("say why: what you found that this should fix")
	case c.Namespace != "" && !kname.MatchString(c.Namespace), !kname.MatchString(c.Name):
		return failed("namespace and name must be Kubernetes names")
	}
	why, err := s.Cluster.Protected(ctx, func(ns string) bool { return config.Matches(s.Config.Protect.Namespaces, ns) }, s.Config.Protect.Label, c.Namespace, c.Kind, c.Name)
	if err != nil {
		return failed(err.Error())
	}
	if why != "" {
		return refused(why)
	}
	why, err = s.Gates.Check(ctx, c.Change)
	if err != nil {
		return refused("couldn't check " + err.Error())
	}
	if why != "" {
		return refused(why)
	}
	if c.pre != nil {
		why, err := c.pre(ctx)
		if err != nil {
			return failed(err.Error())
		}
		if why != "" {
			return failed(why)
		}
	}
	if err := c.do(ctx); err != nil {
		return failed(err.Error())
	}
	r := Result{Result: "done"}
	if c.settle != nil {
		r.Settled, r.Ready = c.settle(ctx)
	}
	return r
}

type workloadIn struct {
	Ref       string `json:"ref,omitempty" jsonschema:"what this change is for, e.g. an incident or ticket; it's logged with the change"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind" jsonschema:"deployment, statefulset or daemonset"`
	Name      string `json:"name"`
	Why       string `json:"why" jsonschema:"what you found that this should fix"`
}

type podIn struct {
	Ref       string `json:"ref,omitempty" jsonschema:"what this change is for, e.g. an incident or ticket; it's logged with the change"`
	Namespace string `json:"namespace"`
	Name      string `json:"name" jsonschema:"the pod's name"`
	Why       string `json:"why" jsonschema:"what you found that this should fix"`
}

type scaleIn struct {
	Ref       string `json:"ref,omitempty" jsonschema:"what this change is for, e.g. an incident or ticket; it's logged with the change"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind" jsonschema:"deployment or statefulset"`
	Name      string `json:"name"`
	Replicas  int32  `json:"replicas"`
	Why       string `json:"why" jsonschema:"what you found that this should fix"`
}

type nodeIn struct {
	Ref  string `json:"ref,omitempty" jsonschema:"what this change is for, e.g. an incident or ticket; it's logged with the change"`
	Node string `json:"node"`
	Why  string `json:"why" jsonschema:"what you found that this should fix"`
}

func (s *Server) tools(ms *mcp.Server) {
	settle := func(ns, kind, name string) func(context.Context) (string, bool) {
		return func(ctx context.Context) (string, bool) {
			return s.Cluster.Settle(ctx, ns, kind, name, s.Config.Settle.Duration)
		}
	}
	fresh := func(ns, kind, name string) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			why, err := s.Cluster.Hopeless(ctx, ns, kind, name)
			if why != "" {
				why = "a new pod can't fix this: " + why
			}
			return why, err
		}
	}
	destructive := true
	mcp.AddTool(ms, &mcp.Tool{
		Name:        "restart",
		Description: "Roll a deployment's, statefulset's or daemonset's pods (kubectl rollout restart), and wait for them to be ready.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in workloadIn) (*mcp.CallToolResult, Result, error) {
		switch in.Kind {
		case "deployment", "statefulset", "daemonset":
		default:
			return nil, failed("kind must be deployment, statefulset or daemonset"), nil
		}
		return nil, s.act(ctx, callerOf(req), change{
			Change: gate.Change{Action: "restart", Namespace: in.Namespace, Kind: in.Kind, Name: in.Name},
			ref:    in.Ref, why: in.Why,
			pre:    fresh(in.Namespace, in.Kind, in.Name),
			do:     func(ctx context.Context) error { return s.Cluster.Restart(ctx, in.Namespace, in.Kind, in.Name) },
			settle: settle(in.Namespace, in.Kind, in.Name),
		}), nil
	})
	mcp.AddTool(ms, &mcp.Tool{
		Name:        "delete_pod",
		Description: "Delete one pod so its controller makes a fresh one, and wait for its workload to be ready.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in podIn) (*mcp.CallToolResult, Result, error) {
		kind, owner := s.Cluster.Owner(ctx, in.Namespace, in.Name)
		c := change{
			Change: gate.Change{Action: "delete_pod", Namespace: in.Namespace, Kind: "pod", Name: in.Name},
			ref:    in.Ref, why: in.Why,
			pre: func(ctx context.Context) (string, error) {
				ok, err := s.Cluster.Controlled(ctx, in.Namespace, in.Name)
				if err != nil || !ok {
					return "this pod has no controller, so it wouldn't come back", err
				}
				return fresh(in.Namespace, "pod", in.Name)(ctx)
			},
			do: func(ctx context.Context) error { return s.Cluster.DeletePod(ctx, in.Namespace, in.Name) },
		}
		if owner != "" {
			c.settle = settle(in.Namespace, kind, owner)
		}
		return nil, s.act(ctx, callerOf(req), c), nil
	})
	mcp.AddTool(ms, &mcp.Tool{
		Name:        "scale",
		Description: "Set a deployment's or statefulset's replicas, and wait for them to be ready.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in scaleIn) (*mcp.CallToolResult, Result, error) {
		switch in.Kind {
		case "deployment", "statefulset":
		default:
			return nil, failed("kind must be deployment or statefulset"), nil
		}
		return nil, s.act(ctx, callerOf(req), change{
			Change: gate.Change{Action: "scale", Namespace: in.Namespace, Kind: in.Kind, Name: in.Name, Replicas: in.Replicas},
			ref:    in.Ref, why: in.Why,
			pre: func(ctx context.Context) (string, error) {
				have, err := s.Cluster.Replicas(ctx, in.Namespace, in.Kind, in.Name)
				switch {
				case err != nil:
					return "", err
				case in.Replicas < 0:
					return "replicas can't be negative", nil
				case in.Replicas == have:
					return fmt.Sprintf("it already wants %d; scaling isn't a way to restart", have), nil
				case in.Replicas > have:
					// More pods that can't start help nobody.
					return fresh(in.Namespace, in.Kind, in.Name)(ctx)
				}
				return "", nil
			},
			do: func(ctx context.Context) error {
				return s.Cluster.Scale(ctx, in.Namespace, in.Kind, in.Name, in.Replicas)
			},
			settle: settle(in.Namespace, in.Kind, in.Name),
		}), nil
	})
	for _, on := range []bool{true, false} {
		name, desc := "cordon", "Stop new pods from being scheduled on a node. Running pods stay."
		if !on {
			name, desc = "uncordon", "Allow new pods on a node again."
		}
		mcp.AddTool(ms, &mcp.Tool{Name: name, Description: desc}, func(ctx context.Context, req *mcp.CallToolRequest, in nodeIn) (*mcp.CallToolResult, Result, error) {
			return nil, s.act(ctx, callerOf(req), change{
				Change: gate.Change{Action: name, Kind: "node", Name: in.Node},
				ref:    in.Ref, why: in.Why,
				do: func(ctx context.Context) error { return s.Cluster.Cordon(ctx, in.Node, on) },
			}), nil
		})
	}
	readOnly := true
	mcp.AddTool(ms, &mcp.Tool{
		Name:        "gates",
		Description: "What kheck never changes, and the rules each change must pass.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
	}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, Rules, error) {
		return nil, Rules{Protect: s.Config.Protect, Gates: s.Config.Gates}, nil
	})
}

// Rules are what the gates tool shows.
type Rules struct {
	Protect config.Protect `json:"protect"`
	Gates   []config.Gate  `json:"gates"`
}
