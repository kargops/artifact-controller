package generator

import (
	"strings"
	"testing"
)

var in = Input{
	Identity:  map[string]string{"gitRef": "v1.4.2", "platform": "windows"},
	Params:    map[string]string{"runner": "large"},
	SpecHash:  "sha256:abcd1234",
	Key:       "clients/sha256:abcd1234",
	Name:      "game-client",
	Namespace: "builds",
	Class:     "s3-game-clients",
	Attempt:   2,
}

func TestRenderKey(t *testing.T) {
	key, err := RenderKey("clients/{{ .SpecHash }}", in)
	if err != nil {
		t.Fatal(err)
	}
	if key != "clients/sha256:abcd1234" {
		t.Fatalf("unexpected key %q", key)
	}
}

func TestRenderKeyEmptyFails(t *testing.T) {
	if _, err := RenderKey("{{ .Params.missing }}", in); err == nil {
		t.Fatal("expected missing key error")
	}
}

func TestRenderTemplate(t *testing.T) {
	raw := []byte(`{
		"apiVersion": "batch/v1",
		"kind": "Job",
		"spec": {
			"backoffLimit": 0,
			"template": {"spec": {"containers": [{
				"name": "build",
				"image": "builder:latest",
				"args": ["--ref", "{{ .Identity.gitRef }}", "--stamp", "{{ .SpecHash }}", "--attempt", "{{ .Attempt }}"]
			}]}}
		}
	}`)
	u, err := RenderTemplate(raw, in)
	if err != nil {
		t.Fatal(err)
	}
	if u.GetKind() != "Job" || u.GetAPIVersion() != "batch/v1" {
		t.Fatalf("gvk lost: %s %s", u.GetAPIVersion(), u.GetKind())
	}
	args, _, err := nestedStringSlice(u.Object, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"v1.4.2", "sha256:abcd1234", "--attempt 2"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rendered args missing %q: %s", want, joined)
		}
	}
	// Numbers survive as numbers.
	spec := u.Object["spec"].(map[string]interface{})
	if _, ok := spec["backoffLimit"].(float64); !ok {
		t.Fatalf("backoffLimit type changed: %T", spec["backoffLimit"])
	}
}

func nestedStringSlice(obj map[string]interface{}, path ...string) ([]string, bool, error) {
	cur := interface{}(obj)
	for _, p := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false, nil
		}
		cur = m[p]
	}
	containers, ok := cur.([]interface{})
	if !ok || len(containers) == 0 {
		return nil, false, nil
	}
	c0 := containers[0].(map[string]interface{})
	rawArgs := c0["args"].([]interface{})
	out := make([]string, 0, len(rawArgs))
	for _, a := range rawArgs {
		out = append(out, a.(string))
	}
	return out, true, nil
}

func TestRenderTemplateMissingFieldFails(t *testing.T) {
	raw := []byte(`{"apiVersion":"v1","kind":"ConfigMap","data":{"x":"{{ .Identity.nope }}"}}`)
	if _, err := RenderTemplate(raw, in); err == nil {
		t.Fatal("expected error for missing identity key")
	}
}

func TestRenderTemplateRequiresGVK(t *testing.T) {
	if _, err := RenderTemplate([]byte(`{"kind":"Job"}`), in); err == nil {
		t.Fatal("expected error for missing apiVersion")
	}
}
