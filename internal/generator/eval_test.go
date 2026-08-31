package generator

import "testing"

func mustEval(t *testing.T) *Evaluator {
	t.Helper()
	e, err := NewEvaluator()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEvalArgoStyle(t *testing.T) {
	e := mustEval(t)
	obj := map[string]interface{}{
		"status": map[string]interface{}{"phase": "Succeeded"},
	}
	ok, err := e.EvalBool(`object.status.phase == 'Succeeded'`, obj)
	if err != nil || !ok {
		t.Fatalf("want true, got %v err=%v", ok, err)
	}
	ok, err = e.EvalBool(`status.phase in ['Failed', 'Error']`, obj)
	if err != nil || ok {
		t.Fatalf("want false, got %v err=%v", ok, err)
	}
}

func TestEvalTektonStyle(t *testing.T) {
	e := mustEval(t)
	obj := map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Succeeded", "status": "False", "reason": "Failed"},
			},
		},
	}
	ok, err := e.EvalBool(`status.conditions.exists(c, c.type == 'Succeeded' && c.status == 'False')`, obj)
	if err != nil || !ok {
		t.Fatalf("want true, got %v err=%v", ok, err)
	}
}

func TestEvalMissingFieldErrorsAndIsNotMatched(t *testing.T) {
	e := mustEval(t)
	obj := map[string]interface{}{"metadata": map[string]interface{}{"name": "x"}}
	ok, err := e.EvalBool(`object.status.phase == 'Succeeded'`, obj)
	if ok {
		t.Fatal("must not match when status is absent")
	}
	if err == nil {
		t.Fatal("expected evaluation error for absent field")
	}
}

func TestEvalNonBoolFails(t *testing.T) {
	e := mustEval(t)
	if _, err := e.EvalBool(`'a string'`, map[string]interface{}{}); err == nil {
		t.Fatal("expected non-bool error")
	}
}

func TestEvalCompileErrorSurfaces(t *testing.T) {
	e := mustEval(t)
	if _, err := e.EvalBool(`this is not CEL`, map[string]interface{}{}); err == nil {
		t.Fatal("expected compile error")
	}
}
