package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestQuery(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("query") {
		case "vec":
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]},{"metric":{},"value":[1,"0"]}]}}`)
		case "one":
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"scalar","result":[1,"2.5"]}}`)
		case "range":
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"status":"error","error":"parse error"}`)
		}
	}))
	defer ts.Close()
	c := New(ts.URL + "/")
	ctx := context.Background()
	if v, err := c.Query(ctx, "vec"); err != nil || !slices.Equal(v, []float64{1, 0}) {
		t.Errorf("vector %v %v", v, err)
	}
	if v, err := c.Query(ctx, "one"); err != nil || !slices.Equal(v, []float64{2.5}) {
		t.Errorf("scalar %v %v", v, err)
	}
	for q, want := range map[string]string{"range": "isn't a yes or no", "(": "parse error"} {
		if _, err := c.Query(ctx, q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
}
