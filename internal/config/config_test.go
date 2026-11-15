package config

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	c, err := Parse([]byte(`
tremolo: {url: http://tremolo}
protect: {namespaces: [kube-system, "cert-*"]}
gates:
  - name: one node at a time
    actions: [cordon]
    require: {controlPlane: false, maxCordoned: 0}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Settle.Duration != 90*time.Second || c.Protect.Label != "kheck/protected" {
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
		`{}`: "tremolo.url is required",
		`{tremolo: {url: x}, gates: [{actions: [cordon]}]}`:                                          "needs a name",
		`{tremolo: {url: x}, gates: [{name: g, actions: [reboot]}]}`:                                 `no action "reboot"`,
		`{tremolo: {url: x}, gates: [{name: g, require: {minReplicas: 3, maxReplicas: 1}}]}`:         "minReplicas is more",
		`{tremolo: {url: x}, gates: [{name: g, require: {maxCordonned: 1}}]}`:                        "unknown field",
		`{tremolo: {url: x}, protect: {namespaces: ["["]}}`:                                          "syntax error",
		`{tremolo: {url: x}, settle: soon}`:                                                          "invalid duration",
		`{tremolo: {url: x}, gates: [{name: g, namespaces: ["a["], require: {controlPlane: true}}]}`: "syntax error",
	} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
}
