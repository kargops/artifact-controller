package ci

import (
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestChartTemplatesHaveNoDuplicateKeys guards a failure mode helm lint does
// not catch: a duplicate mapping key is valid YAML — the last one silently
// wins — so a second `rules:` in a Role renders a Role missing the first set,
// with no error anywhere. That is how an RBAC rule can vanish between writing
// it and applying it.
func helmTemplate(t *testing.T, extra ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	args := []string{"template", "dupcheck", "../charts/artifact-controller",
		"-n", "artifact-system", "-f", "../charts/artifact-controller/ci-values.yaml"}
	args = append(args, extra...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return string(out)
}

func TestChartTemplatesHaveNoDuplicateKeys(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	out := helmTemplate(t)

	dec := yaml.NewDecoder(strings.NewReader(out))
	// yaml.v3 reports duplicate keys as an error only in strict mode, which
	// is what KnownFields gives us via decoding into a map node.
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			break
		}
		if err := checkNode(&node, ""); err != nil {
			t.Error(err)
		}
	}
}

func checkNode(n *yaml.Node, path string) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := checkNode(c, path); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if seen[key] {
				return &duplicateKeyError{path: path, key: key, line: n.Content[i].Line}
			}
			seen[key] = true
			if err := checkNode(n.Content[i+1], path+"."+key); err != nil {
				return err
			}
		}
	}
	return nil
}

type duplicateKeyError struct {
	path, key string
	line      int
}

func TestMetricsServiceRendersWhenEnabled(t *testing.T) {
	out := helmTemplate(t)
	docs := splitDocs(out)
	svc := findKindNamed(t, docs, "Service", "dupcheck-artifact-controller-metrics")
	if got := yamlString(svc, "spec", "selector", "app.kubernetes.io/name"); got != "artifact-controller" {
		t.Errorf("metrics Service selector app.kubernetes.io/name = %q", got)
	}
	if got := yamlString(svc, "spec", "selector", "app.kubernetes.io/instance"); got != "dupcheck" {
		t.Errorf("metrics Service selector app.kubernetes.io/instance = %q", got)
	}
	ports, _ := yamlSeq(svc, "spec", "ports")
	if len(ports) != 1 {
		t.Fatalf("metrics Service ports = %d, want 1", len(ports))
	}
	if yamlString(ports[0], "name") != "metrics" {
		t.Errorf("port name = %q, want metrics", yamlString(ports[0], "name"))
	}
	if yamlString(ports[0], "targetPort") != "metrics" {
		t.Errorf("targetPort = %q, want metrics", yamlString(ports[0], "targetPort"))
	}
	if findKind(docs, "ServiceMonitor") != nil {
		t.Error("ServiceMonitor rendered by default; want absent (CRD not required)")
	}
}

func TestMetricsServiceAbsentWhenDisabled(t *testing.T) {
	out := helmTemplate(t, "--set", "metrics.enabled=false")
	docs := splitDocs(out)
	if findKindNamed(t, docs, "Service", "dupcheck-artifact-controller-metrics") != nil {
		t.Fatal("metrics Service rendered with metrics.enabled=false")
	}
	if findKind(docs, "ServiceMonitor") != nil {
		t.Error("ServiceMonitor rendered with metrics.enabled=false")
	}
}

func TestServiceMonitorRendersWhenEnabled(t *testing.T) {
	out := helmTemplate(t, "--set", "metrics.serviceMonitor.enabled=true",
		"--set", "metrics.serviceMonitor.labels.release=kube-prometheus-stack")
	docs := splitDocs(out)
	sm := findKind(docs, "ServiceMonitor")
	if sm == nil {
		t.Fatal("ServiceMonitor missing when metrics.serviceMonitor.enabled=true")
	}
	if yamlString(sm, "metadata", "labels", "release") != "kube-prometheus-stack" {
		t.Errorf("ServiceMonitor extra label release not applied")
	}
	if yamlString(sm, "spec", "selector", "matchLabels", "app.kubernetes.io/component") != "metrics" {
		t.Errorf("ServiceMonitor selector missing component=metrics")
	}
	eps, _ := yamlSeq(sm, "spec", "endpoints")
	if len(eps) != 1 || yamlString(eps[0], "port") != "metrics" || yamlString(eps[0], "scheme") != "http" {
		t.Errorf("ServiceMonitor endpoint = %#v, want port=metrics scheme=http", eps)
	}
	// A ServiceMonitor without the Service is a scrape of nothing.
	if findKindNamed(t, docs, "Service", "dupcheck-artifact-controller-metrics") == nil {
		t.Fatal("metrics Service missing alongside ServiceMonitor")
	}
}

func splitDocs(rendered string) []map[string]any {
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

func findKind(docs []map[string]any, kind string) map[string]any {
	for _, d := range docs {
		if d["kind"] == kind {
			return d
		}
	}
	return nil
}

func findKindNamed(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] != kind {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		if meta != nil && meta["name"] == name {
			return d
		}
	}
	return nil
}

func yamlString(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[p]
	}
	switch v := cur.(type) {
	case string:
		return v
	case int:
		return itoa(v)
	default:
		return ""
	}
}

func yamlSeq(m map[string]any, path ...string) ([]map[string]any, bool) {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = obj[p]
	}
	seq, ok := cur.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(seq))
	for _, item := range seq {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, true
}

func (e *duplicateKeyError) Error() string {
	return "duplicate key " + e.path + "." + e.key + " in rendered chart (line " +
		itoa(e.line) + "): the later value silently replaces the earlier one"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
