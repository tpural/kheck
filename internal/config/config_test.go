package config

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	c, err := Parse([]byte(`
callers: ["system:serviceaccount:agents:*"]
protect: {namespaces: [kube-system, "cert-*"]}
gates:
  - name: one node at a time
    actions: [cordon]
    require: {controlPlane: false, maxCordoned: 0}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Settle.Duration != 90*time.Second || c.Protect.Label != "kheck/protected" || c.Audiences[0] != "kheck" {
		t.Errorf("defaults %+v", c)
	}
	r := c.Gates[0].Require
	if r.ControlPlane == nil || *r.ControlPlane || r.MaxCordoned == nil || *r.MaxCordoned != 0 || r.OtherNodesReady != nil {
		t.Errorf("require %+v", r)
	}
	if !Matches(c.Protect.Namespaces, "cert-manager") || Matches(c.Protect.Namespaces, "default") {
		t.Error("globs")
	}

	for in, want := range map[string]string{
		`{}`: "callers is required",
		`{callers: [x], gates: [{name: g, require: {query: up}}]}`:                              "needs metrics.url",
		`{callers: [x], gates: [{actions: [cordon]}]}`:                                          "needs a name",
		`{callers: [x], gates: [{name: g, actions: [reboot]}]}`:                                 `no action "reboot"`,
		`{callers: [x], gates: [{name: g, require: {minReplicas: 3, maxReplicas: 1}}]}`:         "minReplicas is more",
		`{callers: [x], gates: [{name: g, require: {maxCordonned: 1}}]}`:                        "unknown field",
		`{callers: [x], protect: {namespaces: ["["]}}`:                                          "syntax error",
		`{callers: [x], settle: soon}`:                                                          "invalid duration",
		`{callers: [x], gates: [{name: g, namespaces: ["a["], require: {controlPlane: true}}]}`: "syntax error",
	} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
}
