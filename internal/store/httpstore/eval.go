package httpstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"
)

// renderTemplate expands the {{ .Key }} placeholder in a URL. The key is the
// only value a request needs — everything identity-derived is already folded
// into it by the class's keyTemplate.
func renderTemplate(tmpl, key string) (string, error) {
	t, err := template.New("url").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse url template %q: %w", tmpl, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, struct{ Key string }{Key: key}); err != nil {
		return "", fmt.Errorf("execute url template %q: %w", tmpl, err)
	}
	return buf.String(), nil
}

// parseJSON exposes a decoded body to expressions when the response is JSON,
// and nil otherwise. Stores that answer with metadata documents are the reason
// this exists; a non-JSON body simply leaves `json` unusable.
func parseJSON(body string) interface{} {
	if body == "" {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return nil
	}
	return v
}

func (d *driver) activation(r *response) map[string]interface{} {
	j := r.json
	if j == nil {
		// CEL cannot bind a nil dyn value; an empty map keeps expressions
		// that touch `json` evaluable (they simply find nothing).
		j = map[string]interface{}{}
	}
	return map[string]interface{}{
		"code":    int64(r.code),
		"headers": r.headers,
		"body":    r.body,
		"json":    j,
	}
}

func (d *driver) evalBool(expr string, r *response, fallback func(*response) bool) (bool, error) {
	if expr == "" {
		return fallback(r), nil
	}
	out, err := d.eval(expr, r)
	if err != nil {
		return false, err
	}
	b, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("expression %q returned %T, want bool", expr, out)
	}
	return b, nil
}

// evalString tolerates a missing value: a store that has no digest or stamp
// for an object should report an empty string, not fail the observation.
func (d *driver) evalString(expr string, r *response) (string, error) {
	out, err := d.eval(expr, r)
	if err != nil {
		return "", nil
	}
	switch v := out.(type) {
	case string:
		return v, nil
	case nil:
		return "", nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

func (d *driver) eval(expr string, r *response) (interface{}, error) {
	ast, iss := d.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("compile %q: %w", expr, iss.Err())
	}
	prg, err := d.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("program %q: %w", expr, err)
	}
	val, _, err := prg.Eval(d.activation(r))
	if err != nil {
		return nil, fmt.Errorf("evaluate %q: %w", expr, err)
	}
	return val.Value(), nil
}
