// Package tremolo is the part of tremolo's API kheck uses: who a token is,
// recording actions on an investigation, and metrics queries. Every call is
// made with the caller's own token, so tremolo's scopes, budgets and audit
// log apply to the caller, and kheck holds no tokens.
package tremolo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to tremolo.
type Client struct {
	URL  string
	HTTP *http.Client
}

// New returns a client for tremolo at base.
func New(base string) *Client {
	return &Client{URL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Error is tremolo answering with an error.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("%d: %s", e.Code, e.Msg) }

// Refused reports whether tremolo refused an action: paused, over a budget,
// a protected host, or a token without the scope.
func Refused(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Code == http.StatusConflict || e.Code == http.StatusLocked || e.Code == http.StatusForbidden)
}

func (c *Client) do(ctx context.Context, token, method, path string, q url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := c.URL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &Error{resp.StatusCode, e.Error}
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

// Me is the name tremolo knows a token by: the token's own name, e.g.
// upkeeper, or its user's.
func (c *Client) Me(ctx context.Context, token string) (string, error) {
	var me struct {
		Name  string `json:"name"`
		Token *struct {
			Name string `json:"name"`
		} `json:"token"`
	}
	if err := c.do(ctx, token, http.MethodGet, "/api/me", nil, nil, &me); err != nil {
		return "", err
	}
	if me.Token != nil && me.Token.Name != "" {
		return me.Token.Name, nil
	}
	if me.Name == "" {
		return "", errors.New("tremolo doesn't know this token")
	}
	return me.Name, nil
}

// Action is a change about to be made.
type Action struct {
	Action     string `json:"action"`
	Host       string `json:"host,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Target     string `json:"target"`
	Disruptive bool   `json:"disruptive"`
	Why        string `json:"why,omitempty"`
}

// BeginAction records an action before it's made; tremolo may refuse it.
func (c *Client) BeginAction(ctx context.Context, token string, inv int64, a Action) (int64, error) {
	var e struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, token, http.MethodPost, fmt.Sprintf("/api/investigations/%d/actions", inv), nil, a, &e)
	return e.ID, err
}

// EndAction records how an action went.
func (c *Client) EndAction(ctx context.Context, token string, inv, event int64, result string) error {
	return c.do(ctx, token, http.MethodPost, fmt.Sprintf("/api/investigations/%d/actions/%d", inv, event), nil, map[string]string{"result": result}, nil)
}

// Query runs an instant PromQL query and returns each sample's value.
func (c *Client) Query(ctx context.Context, token, promql string) ([]float64, error) {
	var res struct {
		Data struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := c.do(ctx, token, http.MethodGet, "/api/metrics/query", url.Values{"query": {promql}}, nil, &res); err != nil {
		return nil, err
	}
	var values [][]any
	switch res.Data.ResultType {
	case "scalar":
		var v []any
		if err := json.Unmarshal(res.Data.Result, &v); err != nil {
			return nil, err
		}
		values = [][]any{v}
	default:
		var vec []struct {
			Value []any `json:"value"`
		}
		if err := json.Unmarshal(res.Data.Result, &vec); err != nil {
			return nil, err
		}
		for _, s := range vec {
			values = append(values, s.Value)
		}
	}
	out := make([]float64, 0, len(values))
	for _, v := range values {
		if len(v) != 2 {
			continue
		}
		s, _ := v[1].(string)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}
