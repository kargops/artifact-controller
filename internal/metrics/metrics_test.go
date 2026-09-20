package metrics

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kargops/artifact-controller/internal/store"
)

func TestCollectAndCompareFailedStoreAndDrift(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	c := New(reg)

	c.RecordStore("s3", OpObserve, errors.New("boom"), 50*time.Millisecond)
	c.RecordVerification(VerifyDrift)
	c.RecordGenerator(GeneratorFailed)
	c.RecordFailureBudgetExhausted()

	want := `
# HELP artifact_controller_failure_budget_exhausted_total Times an Artifact exhausted its consecutive-failure budget and became Degraded.
# TYPE artifact_controller_failure_budget_exhausted_total counter
artifact_controller_failure_budget_exhausted_total 1
# HELP artifact_controller_generator_runs_total Generator run terminal outcomes (and unrecognized in-progress classifications).
# TYPE artifact_controller_generator_runs_total counter
artifact_controller_generator_runs_total{result="failed"} 1
# HELP artifact_controller_store_operation_errors_total Failed store Observe/Delete calls by driver and operation.
# TYPE artifact_controller_store_operation_errors_total counter
artifact_controller_store_operation_errors_total{driver="s3",op="observe"} 1
# HELP artifact_controller_store_operations_total Store Observe/Delete calls by driver and operation.
# TYPE artifact_controller_store_operations_total counter
artifact_controller_store_operations_total{driver="s3",op="observe"} 1
# HELP artifact_controller_verification_outcomes_total Store verification outcomes. key_conflict and drift should stay near-flat.
# TYPE artifact_controller_verification_outcomes_total counter
artifact_controller_verification_outcomes_total{result="drift"} 1
`
	if err := testutil.CollectAndCompare(reg, strings.NewReader(want),
		"artifact_controller_store_operations_total",
		"artifact_controller_store_operation_errors_total",
		"artifact_controller_generator_runs_total",
		"artifact_controller_verification_outcomes_total",
		"artifact_controller_failure_budget_exhausted_total",
	); err != nil {
		t.Fatal(err)
	}
}

func TestLabelSetsAreBounded(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	c := New(reg)
	c.RecordStore("http", OpDelete, nil, time.Millisecond)
	c.RecordStore("oci", OpObserve, errors.New("x"), time.Millisecond)
	c.RecordGenerator(GeneratorSucceededWithoutArtifact)
	c.RecordGenerator(GeneratorUnrecognized)
	c.RecordGenerator(GeneratorProgressDeadlineExceeded)
	c.RecordVerification(VerifyKeyConflict)
	c.RecordVerification(VerifyOK)

	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]map[string]struct{}{
		"artifact_controller_store_operations_total":           {"driver": {}, "op": {}},
		"artifact_controller_store_operation_errors_total":     {"driver": {}, "op": {}},
		"artifact_controller_store_operation_duration_seconds": {"driver": {}, "op": {}},
		"artifact_controller_generator_runs_total":             {"result": {}},
		"artifact_controller_verification_outcomes_total":      {"result": {}},
		"artifact_controller_failure_budget_exhausted_total":   {},
	}
	forbidden := []string{"artifact", "name", "namespace", "key", "hash", "spec_hash", "specHash"}
	for _, fam := range fams {
		want, ok := allowed[fam.GetName()]
		if !ok {
			t.Fatalf("unexpected metric %s", fam.GetName())
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				n := lp.GetName()
				if n == "le" {
					continue // histogram buckets
				}
				if _, ok := want[n]; !ok {
					t.Errorf("%s: unexpected label %q", fam.GetName(), n)
				}
				for _, f := range forbidden {
					if strings.EqualFold(n, f) {
						t.Errorf("%s: forbidden identity label %q", fam.GetName(), n)
					}
				}
			}
		}
	}
}

type stubDriver struct {
	observeErr error
	deleteErr  error
}

func (s stubDriver) Observe(context.Context, string) (store.Observation, error) {
	return store.Observation{Exists: true, Digest: "abc"}, s.observeErr
}
func (s stubDriver) Delete(context.Context, string) error { return s.deleteErr }

func TestInstrumentRecordsObserveError(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	c := New(reg)
	d := c.Instrument(stubDriver{observeErr: errors.New("denied")}, "artifactory")
	_, err := d.Observe(context.Background(), "should-not-become-a-label")
	if err == nil {
		t.Fatal("expected observe error")
	}
	if got := testutil.ToFloat64(c.storeErrs.WithLabelValues("artifactory", OpObserve)); got != 1 {
		t.Fatalf("store errors = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.storeOps.WithLabelValues("artifactory", OpObserve)); got != 1 {
		t.Fatalf("store ops = %v, want 1", got)
	}
}

func TestNilCollectorsAreNoop(t *testing.T) {
	var c *Collectors
	c.RecordStore("s3", OpObserve, io.EOF, time.Second)
	c.RecordGenerator(GeneratorFailed)
	c.RecordVerification(VerifyDrift)
	c.RecordFailureBudgetExhausted()
	d := c.Instrument(stubDriver{}, "s3")
	if _, ok := d.(stubDriver); !ok {
		t.Fatalf("nil Instrument should return inner unchanged, got %T", d)
	}
}
