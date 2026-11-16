// Package metrics runs instant PromQL queries against a Prometheus-compatible
// API, for query gates.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client queries a Prometheus-compatible API.
type Client struct {
	URL  string
	HTTP *http.Client
}

// New returns a client for the API at base, e.g. http://prometheus:9090.
func New(base string) *Client {
	return &Client{URL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Query runs an instant query and returns each sample's value.
func (c *Client) Query(ctx context.Context, promql string) ([]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/api/v1/query?"+url.Values{"query": {promql}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var res struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if res.Status != "success" {
		return nil, fmt.Errorf("%s: %s", resp.Status, res.Error)
	}
	var values [][]any
	switch res.Data.ResultType {
	case "scalar":
		var v []any
		if err := json.Unmarshal(res.Data.Result, &v); err != nil {
			return nil, err
		}
		values = [][]any{v}
	case "vector":
		var vec []struct {
			Value []any `json:"value"`
		}
		if err := json.Unmarshal(res.Data.Result, &vec); err != nil {
			return nil, err
		}
		for _, s := range vec {
			values = append(values, s.Value)
		}
	default:
		return nil, fmt.Errorf("a %s isn't a yes or no; use an instant query", res.Data.ResultType)
	}
	out := make([]float64, 0, len(values))
	for _, v := range values {
		if len(v) != 2 {
			continue
		}
		s, _ := v[1].(string)
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			out = append(out, f)
		}
	}
	return out, nil
}
