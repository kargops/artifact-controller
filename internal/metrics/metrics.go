// Package metrics exposes domain Prometheus collectors for store calls,
// generator terminal outcomes, verification, and failure-budget exhaustion.
//
// Collectors register on the caller-supplied Registerer (production uses
// controller-runtime's pkg/metrics.Registry so they share the existing
// endpoint). Label sets are closed: driver and op are class/driver vocabulary,
// result is a fixed enum. Artifact name, spec hash, and store key are never
// labels.
package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/kargops/artifact-controller/internal/store"
)

// Closed label values. Anything else is a programming error, not a new series
// we want to invent at scrape time.
const (
	OpObserve = "observe"
	OpDelete  = "delete"

	GeneratorSucceeded                = "succeeded"
	GeneratorFailed                   = "failed"
	GeneratorSucceededWithoutArtifact = "succeeded_without_artifact"
	GeneratorUnrecognized             = "unrecognized"
	GeneratorProgressDeadlineExceeded = "progress_deadline_exceeded"

	VerifyOK          = "ok"
	VerifyKeyConflict = "key_conflict"
	VerifyDrift       = "drift"
)

// Collectors holds the domain metrics. A nil receiver is a no-op so the
// reconciler can call through without a nil check at every site.
type Collectors struct {
	storeOps  *prometheus.CounterVec
	storeErrs *prometheus.CounterVec
	storeDur  *prometheus.HistogramVec
	generator *prometheus.CounterVec
	verify    *prometheus.CounterVec
	budgetExh prometheus.Counter
}

// New registers collectors on reg. Panics if the same descriptors are already
// registered on that registry.
func New(reg prometheus.Registerer) *Collectors {
	c := &Collectors{
		storeOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "artifact_controller_store_operations_total",
			Help: "Store Observe/Delete calls by driver and operation.",
		}, []string{"driver", "op"}),
		storeErrs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "artifact_controller_store_operation_errors_total",
			Help: "Failed store Observe/Delete calls by driver and operation.",
		}, []string{"driver", "op"}),
		storeDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "artifact_controller_store_operation_duration_seconds",
			Help:    "Store Observe/Delete wall time by driver and operation.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"driver", "op"}),
		generator: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "artifact_controller_generator_runs_total",
			Help: "Generator run terminal outcomes (and unrecognized in-progress classifications).",
		}, []string{"result"}),
		verify: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "artifact_controller_verification_outcomes_total",
			Help: "Store verification outcomes. key_conflict and drift should stay near-flat.",
		}, []string{"result"}),
		budgetExh: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "artifact_controller_failure_budget_exhausted_total",
			Help: "Times an Artifact exhausted its consecutive-failure budget and became Degraded.",
		}),
	}
	reg.MustRegister(c.storeOps, c.storeErrs, c.storeDur, c.generator, c.verify, c.budgetExh)
	return c
}

var (
	defaultOnce sync.Once
	defaultColl *Collectors
)

// Default is the process-wide collectors on controller-runtime's registry.
func Default() *Collectors {
	defaultOnce.Do(func() {
		defaultColl = New(ctrlmetrics.Registry)
	})
	return defaultColl
}

// Instrument wraps d so Observe/Delete record store metrics. A nil Collectors
// returns d unchanged. The driver label is the class's store.driver name.
func (c *Collectors) Instrument(d store.Driver, driver string) store.Driver {
	if c == nil || d == nil {
		return d
	}
	return &instrumented{inner: d, driver: driver, m: c}
}

func (c *Collectors) RecordStore(driver, op string, err error, d time.Duration) {
	if c == nil {
		return
	}
	c.storeOps.WithLabelValues(driver, op).Inc()
	c.storeDur.WithLabelValues(driver, op).Observe(d.Seconds())
	if err != nil {
		c.storeErrs.WithLabelValues(driver, op).Inc()
	}
}

func (c *Collectors) RecordGenerator(result string) {
	if c == nil {
		return
	}
	c.generator.WithLabelValues(result).Inc()
}

func (c *Collectors) RecordVerification(result string) {
	if c == nil {
		return
	}
	c.verify.WithLabelValues(result).Inc()
}

func (c *Collectors) RecordFailureBudgetExhausted() {
	if c == nil {
		return
	}
	c.budgetExh.Inc()
}

type instrumented struct {
	inner  store.Driver
	driver string
	m      *Collectors
}

func (d *instrumented) Observe(ctx context.Context, key string) (store.Observation, error) {
	start := time.Now()
	obs, err := d.inner.Observe(ctx, key)
	d.m.RecordStore(d.driver, OpObserve, err, time.Since(start))
	return obs, err
}

func (d *instrumented) Delete(ctx context.Context, key string) error {
	start := time.Now()
	err := d.inner.Delete(ctx, key)
	d.m.RecordStore(d.driver, OpDelete, err, time.Since(start))
	return err
}
