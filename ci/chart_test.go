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
func TestChartTemplatesHaveNoDuplicateKeys(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	out, err := exec.Command("helm", "template", "dupcheck",
		"../charts/artifact-controller", "-n", "artifact-system",
		"-f", "../charts/artifact-controller/ci-values.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(out)))
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
