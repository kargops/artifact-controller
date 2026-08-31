// Package generator renders generator run objects from class templates and
// interprets their status engine-agnostically via CEL.
package generator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Input is the data available to the key template and every string leaf of a
// generator template.
type Input struct {
	Identity map[string]string
	Params   map[string]string
	// SpecHash is the canonical content address ("sha256:<hex>").
	SpecHash string
	// SpecHex is the bare hex of SpecHash, for contexts that forbid ':'
	// (OCI tags).
	SpecHex   string
	Key       string
	Name      string
	Namespace string
	Class     string
	Attempt   int32
}

// RenderKey renders a store-key template.
func RenderKey(tmpl string, in Input) (string, error) {
	out, err := renderString(tmpl, in)
	if err != nil {
		return "", fmt.Errorf("render key template: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("key template rendered to an empty key")
	}
	return out, nil
}

// RenderTemplate renders a generator object template (raw JSON from the class)
// by expanding Go template syntax in every string leaf. The caller is
// responsible for setting name/namespace/ownerRef.
func RenderTemplate(raw []byte, in Input) (*unstructured.Unstructured, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("generator template is empty")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("generator template is not a valid object: %w", err)
	}
	rendered, err := walk(obj, in)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: rendered.(map[string]interface{})}
	if u.GetAPIVersion() == "" || u.GetKind() == "" {
		return nil, fmt.Errorf("generator template must set apiVersion and kind")
	}
	return u, nil
}

func walk(v interface{}, in Input) (interface{}, error) {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			r, err := walk(val, in)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			r, err := walk(val, in)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case string:
		if !strings.Contains(t, "{{") {
			return t, nil
		}
		return renderString(t, in)
	default:
		return v, nil
	}
}

func renderString(tmpl string, in Input) (string, error) {
	t, err := template.New("t").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", tmpl, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, in); err != nil {
		return "", fmt.Errorf("execute template %q: %w", tmpl, err)
	}
	return buf.String(), nil
}
