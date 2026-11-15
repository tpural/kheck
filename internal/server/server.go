// Package server is kheck's MCP server: the changes agents may ask for, each
// checked, recorded in tremolo, made, and waited on.
package server

import (
	"context"
	"crypto/sha256"
	"errors"
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
	"github.com/tpural/kheck/internal/tremolo"
)

// Server serves kheck's tools.
type Server struct {
	Config  *config.Config
	Cluster *kube.Cluster
	Tremolo *tremolo.Client
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

// tokenFor is how long a token tremolo knows is trusted before asking again.
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

// verify asks tremolo who a token is: kheck trusts only tokens tremolo
// knows, and acts as them.
func (s *Server) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	k, ok := s.tokens[key]
	s.mu.Unlock()
	if !ok || time.Since(k.at) > tokenFor {
		name, err := s.Tremolo.Me(ctx, token)
		var te *tremolo.Error
		if errors.As(err, &te) && te.Code == http.StatusUnauthorized {
			return nil, auth.ErrInvalidToken
		}
		if err != nil {
			return nil, fmt.Errorf("asking tremolo who this is: %w", err)
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
	return &auth.TokenInfo{UserID: k.name, Expiration: k.at.Add(tokenFor), Extra: map[string]any{"token": token}}, nil
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

type caller struct{ name, token string }

func callerOf(req *mcp.CallToolRequest) caller {
	if req.Extra == nil || req.Extra.TokenInfo == nil {
		return caller{}
	}
	t, _ := req.Extra.TokenInfo.Extra["token"].(string)
	return caller{req.Extra.TokenInfo.UserID, t}
}

var kname = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// change is one request to change something, and how to make it.
type change struct {
	gate.Change
	inv    int64
	why    string
	action tremolo.Action
	// pre refuses what can't work, e.g. a new pod for a bad image.
	pre    func(context.Context) (string, error)
	do     func(context.Context) error
	settle func(context.Context) (string, bool)
}

// act checks a change, records it in tremolo, makes it and waits for it to
// settle. Anything it can't check refuses it.
func (s *Server) act(ctx context.Context, who caller, c change) Result {
	r := s.act1(ctx, who, c)
	s.Log.Info("change", "by", who.name, "investigation", c.inv, "action", c.Action, "namespace", c.Namespace, "kind", c.Kind, "name", c.Name,
		"why", c.why, "result", r.Result, "detail", r.Why)
	return r
}

func (s *Server) act1(ctx context.Context, who caller, c change) Result {
	switch {
	case who.token == "":
		return refused("no caller: kheck acts only for a tremolo token")
	case c.inv <= 0:
		return failed("say which investigation this is for")
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
	why, err = s.Gates.Check(ctx, who.token, c.Change)
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
	c.action.Why = c.why
	ev, err := s.Tremolo.BeginAction(ctx, who.token, c.inv, c.action)
	if tremolo.Refused(err) {
		return refused("tremolo: " + err.Error())
	}
	if err != nil {
		return failed("couldn't record it in tremolo, so nothing was changed: " + err.Error())
	}
	result := "done"
	if err := c.do(ctx); err != nil {
		result = "failed: " + err.Error()
	}
	if err := s.Tremolo.EndAction(context.WithoutCancel(ctx), who.token, c.inv, ev, result); err != nil {
		s.Log.Error("recording how an action went", "investigation", c.inv, "event", ev, "err", err)
	}
	if result != "done" {
		return failed(strings.TrimPrefix(result, "failed: "))
	}
	r := Result{Result: "done"}
	if c.settle != nil {
		r.Settled, r.Ready = c.settle(ctx)
	}
	return r
}

type workloadIn struct {
	Investigation int64  `json:"investigation" jsonschema:"the tremolo investigation this change is for"`
	Namespace     string `json:"namespace"`
	Kind          string `json:"kind" jsonschema:"deployment, statefulset or daemonset"`
	Name          string `json:"name"`
	Why           string `json:"why" jsonschema:"what you found that this should fix"`
}

type podIn struct {
	Investigation int64  `json:"investigation" jsonschema:"the tremolo investigation this change is for"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name" jsonschema:"the pod's name"`
	Why           string `json:"why" jsonschema:"what you found that this should fix"`
}

type scaleIn struct {
	Investigation int64  `json:"investigation" jsonschema:"the tremolo investigation this change is for"`
	Namespace     string `json:"namespace"`
	Kind          string `json:"kind" jsonschema:"deployment or statefulset"`
	Name          string `json:"name"`
	Replicas      int32  `json:"replicas"`
	Why           string `json:"why" jsonschema:"what you found that this should fix"`
}

type nodeIn struct {
	Investigation int64  `json:"investigation" jsonschema:"the tremolo investigation this change is for"`
	Node          string `json:"node"`
	Why           string `json:"why" jsonschema:"what you found that this should fix"`
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
			inv:    in.Investigation, why: in.Why,
			action: tremolo.Action{Action: "rollout restart", Namespace: in.Namespace, Target: in.Kind + "/" + in.Name, Disruptive: true},
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
			inv:    in.Investigation, why: in.Why,
			action: tremolo.Action{Action: "delete pod", Namespace: in.Namespace, Target: "pod/" + in.Name, Disruptive: true},
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
			inv:    in.Investigation, why: in.Why,
			action: tremolo.Action{Action: fmt.Sprintf("scale to %d", in.Replicas), Namespace: in.Namespace, Target: in.Kind + "/" + in.Name, Disruptive: true},
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
				inv:    in.Investigation, why: in.Why,
				action: tremolo.Action{Action: name, Host: in.Node, Target: "node/" + in.Node},
				do:     func(ctx context.Context) error { return s.Cluster.Cordon(ctx, in.Node, on) },
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
