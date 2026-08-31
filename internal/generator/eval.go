package generator

import (
	"fmt"
	"sync"

	"cel.dev/cel-go/cel"
)

// Evaluator compiles and caches CEL programs for generator status
// interpretation. Expressions see two variables: `object` (the full generator
// object) and `status` (shorthand for object.status, an empty map when the
// generator has no status yet).
type Evaluator struct {
	env *cel.Env

	mu    sync.Mutex
	progs map[string]cel.Program
}

func NewEvaluator() (*Evaluator, error) {
	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
		cel.Variable("status", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("create CEL env: %w", err)
	}
	return &Evaluator{env: env, progs: map[string]cel.Program{}}, nil
}

// EvalBool evaluates expr against the generator object. A non-boolean result
// is an error. Callers should treat evaluation errors (e.g. a field that does
// not exist yet) as "not matched".
func (e *Evaluator) EvalBool(expr string, object map[string]interface{}) (bool, error) {
	prg, err := e.program(expr)
	if err != nil {
		return false, err
	}
	status, _ := object["status"].(map[string]interface{})
	if status == nil {
		status = map[string]interface{}{}
	}
	out, _, err := prg.Eval(map[string]interface{}{
		"object": object,
		"status": status,
	})
	if err != nil {
		return false, fmt.Errorf("evaluate %q: %w", expr, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression %q returned %T, want bool", expr, out.Value())
	}
	return b, nil
}

func (e *Evaluator) program(expr string) (cel.Program, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.progs[expr]; ok {
		return p, nil
	}
	ast, iss := e.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("compile %q: %w", expr, iss.Err())
	}
	prg, err := e.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("program %q: %w", expr, err)
	}
	e.progs[expr] = prg
	return prg, nil
}
