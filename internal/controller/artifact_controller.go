// Package controller implements the Artifact reconciler: observe the external
// store, trigger the class's generator when the artifact is missing, verify
// the produced object, and enforce ttl/deleteAfter/deletionPolicy semantics.
//
// Structure and conventions are adapted from fluxcd/source-controller and
// cert-manager (Apache-2.0); see NOTICE.
package controller

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/generator"
	"github.com/kargops/artifact-controller/internal/hash"
	"github.com/kargops/artifact-controller/internal/store"
)

const classIndexKey = ".spec.classRef.name"

// unrecognizedGracePeriod is how long a run may report nothing the class
// describes before that is treated as a misconfigured vocabulary rather than
// a just-created object.
//
// Short on purpose: a healthy engine populates status sub-second, and the
// case this most needs to catch — a class templating a kind whose operator is
// absent or crashlooping, so status never arrives — is one you want reported
// promptly. progressDeadline remains the backstop. A var so tests need not
// wait it out.
var unrecognizedGracePeriod = 15 * time.Second

// ArtifactReconciler reconciles Artifact objects.
type ArtifactReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	Registry                *store.Registry
	Eval                    *generator.Evaluator
	Recorder                record.EventRecorder
	FieldOwner              string
	MaxConcurrentReconciles int

	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time

	controller controller.Controller
	cache      cache.Cache
	mapper     apimeta.RESTMapper
	watched    sync.Map // schema.GroupVersionKind -> struct{}
}

// +kubebuilder:rbac:groups=artifacts.kargops.dev,resources=artifacts,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=artifacts.kargops.dev,resources=artifacts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=artifacts.kargops.dev,resources=artifacts/finalizers,verbs=update
// +kubebuilder:rbac:groups=artifacts.kargops.dev,resources=artifactclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//
// NOTE: permissions on generator run kinds (Argo Workflows, Tekton
// PipelineRuns, Jobs, ...) are deliberately NOT generated here — they are
// installation-specific. Grant them by applying an engine ClusterRole labelled
// artifacts.kargops.dev/aggregate-to-manager=true (see config/rbac/generator-engines/),
// which aggregates into the manager role at runtime.

// SetupWithManager wires the reconciler, the class watch, and the field index.
func (r *ArtifactReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.FieldOwner == "" {
		r.FieldOwner = "artifact-controller"
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &artifactsv1.Artifact{}, classIndexKey,
		func(o client.Object) []string {
			return []string{o.(*artifactsv1.Artifact).Spec.ClassRef.Name}
		}); err != nil {
		return fmt.Errorf("index artifacts by class: %w", err)
	}
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&artifactsv1.Artifact{}).
		Watches(&artifactsv1.ArtifactClass{}, handler.EnqueueRequestsFromMapFunc(r.artifactsForClass)).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxInt(r.MaxConcurrentReconciles, 1)}).
		Build(r)
	if err != nil {
		return err
	}
	r.controller = c
	r.cache = mgr.GetCache()
	r.mapper = mgr.GetRESTMapper()
	return nil
}

func (r *ArtifactReconciler) artifactsForClass(ctx context.Context, obj client.Object) []reconcile.Request {
	var list artifactsv1.ArtifactList
	if err := r.List(ctx, &list, client.MatchingFields{classIndexKey: obj.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list artifacts for class", "class", obj.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

// ensureGeneratorWatch lazily registers an owner-watch for a generator GVK the
// first time a class uses it, so generator status changes requeue the owning
// Artifact without polling.
func (r *ArtifactReconciler) ensureGeneratorWatch(gvk schema.GroupVersionKind) error {
	if _, loaded := r.watched.LoadOrStore(gvk, struct{}{}); loaded {
		return nil
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := r.controller.Watch(source.Kind[client.Object](r.cache, u,
		handler.EnqueueRequestForOwner(r.Scheme, r.mapper, &artifactsv1.Artifact{}, handler.OnlyControllerOwner())))
	if err != nil {
		r.watched.Delete(gvk)
	}
	return err
}

// Reconcile implements the state machine.
func (r *ArtifactReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	obj := &artifactsv1.Artifact{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patcher := patch.NewSerialPatcher(obj, r.Client)
	defer func() {
		if err := patcher.Patch(ctx, obj, r.patchOpts()...); err != nil {
			if !obj.DeletionTimestamp.IsZero() {
				err = kerrors.FilterOut(err, apierrors.IsNotFound)
			}
			if err != nil {
				retErr = kerrors.NewAggregate([]error{retErr, err})
			}
		}
	}()

	if !obj.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, obj)
	}

	if !controllerutil.ContainsFinalizer(obj, artifactsv1.ArtifactFinalizer) {
		controllerutil.AddFinalizer(obj, artifactsv1.ArtifactFinalizer)
		return ctrl.Result{RequeueAfter: 10 * time.Millisecond}, nil
	}

	return r.reconcile(ctx, obj)
}

func (r *ArtifactReconciler) reconcile(ctx context.Context, obj *artifactsv1.Artifact) (ctrl.Result, error) {
	now := r.Now()
	if obj.Status.State == "" {
		obj.Status.State = artifactsv1.StatePending
	}
	obj.Status.ObservedGeneration = obj.Generation

	if obj.Spec.Suspend {
		obj.Status.State = artifactsv1.StateSuspended
		conditions.Delete(obj, fluxmeta.ReconcilingCondition)
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonSuspended, "reconciliation is suspended")
		return ctrl.Result{}, nil
	}

	// Honor the retry annotation: reset the failure budget once per token.
	if tok := obj.GetAnnotations()[artifactsv1.RetryAnnotation]; tok != "" && tok != obj.Status.RetryToken {
		obj.Status.RetryToken = tok
		obj.Status.FailedAttempts = 0
		obj.Status.LastFailureTime = nil
		obj.Status.LastFailureMessage = ""
		conditions.Delete(obj, fluxmeta.StalledCondition)
		r.Recorder.Event(obj, corev1.EventTypeNormal, "RetryRequested", "failure budget reset via retry annotation")
	}

	class := &artifactsv1.ArtifactClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: obj.Spec.ClassRef.Name}, class); err != nil {
		if apierrors.IsNotFound(err) {
			conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonClassNotFound,
				"ArtifactClass %q not found", obj.Spec.ClassRef.Name)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	specHash := hash.Canonical(obj.Spec.Identity)
	in := generator.Input{
		Identity:  obj.Spec.Identity,
		Params:    obj.Spec.Params,
		SpecHash:  specHash,
		SpecHex:   hash.Short(specHash, 64),
		Name:      obj.Name,
		Namespace: obj.Namespace,
		Class:     class.Name,
	}
	key, err := generator.RenderKey(class.KeyTemplate(), in)
	if err != nil {
		markStalled(obj, artifactsv1.ReasonTemplateError, "%s", err)
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonTemplateError, "%s", err)
		// A class edit retriggers via the class watch; no requeue needed.
		return ctrl.Result{}, nil
	}
	in.Key = key
	obj.Status.SpecHash = specHash
	obj.Status.Key = key

	age := now.Sub(obj.CreationTimestamp.Time)

	// TTL: stop reconciling and delete the CR; the finalizer applies the
	// deletion policy to the store object.
	if ttl := obj.Spec.TTL; ttl != nil && ttl.Duration > 0 && age >= ttl.Duration {
		r.Recorder.Eventf(obj, corev1.EventTypeNormal, "TTLExpired",
			"ttl %s reached; deleting Artifact", ttl.Duration)
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	driver, err := r.Registry.DriverFor(ctx, class)
	if err != nil {
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonStoreUnavailable, "%s", err)
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// DeleteAfter: one-shot GC of the store object, then park as Expired.
	// CRD validation rejects deleteAfter on observe-only Artifacts; the policy
	// check here is defense in depth for objects admitted before the rule.
	if da := obj.Spec.DeleteAfter; da != nil && da.Duration > 0 && age >= da.Duration && !obj.ObserveOnly() {
		return r.reconcileExpired(ctx, obj, class, driver, key, specHash, now)
	}

	obs, err := driver.Observe(ctx, key)
	if err != nil {
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonStoreUnavailable, "%s", err)
		return ctrl.Result{RequeueAfter: jittered(30 * time.Second)}, nil
	}

	if obs.Exists {
		return r.reconcileExisting(ctx, obj, class, obs, in, now)
	}
	if obj.ObserveOnly() {
		return r.reconcileMissingObserved(ctx, obj, in)
	}
	return r.reconcileMissing(ctx, obj, class, in, now)
}

// reconcileExisting handles a present store object: verify provenance, mark
// Ready, keep verifying on the interval.
func (r *ArtifactReconciler) reconcileExisting(ctx context.Context, obj *artifactsv1.Artifact, class *artifactsv1.ArtifactClass, obs store.Observation, in generator.Input, now time.Time) (ctrl.Result, error) {
	key, specHash := in.Key, in.SpecHash
	if stamp := obs.Metadata[class.StampMetadataKey()]; stamp != "" && stamp != specHash {
		obj.Status.State = artifactsv1.StateKeyConflict
		conditions.Delete(obj, fluxmeta.ReconcilingCondition)
		conditions.MarkFalse(obj, artifactsv1.ArtifactInStoreCondition, artifactsv1.ReasonKeyConflict,
			"store object at %q is stamped %s", key, stamp)
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonKeyConflict,
			"store object at %q carries provenance stamp %s, expected %s; refusing to adopt or overwrite", key, stamp, specHash)
		r.Recorder.Eventf(obj, corev1.EventTypeWarning, "KeyConflict",
			"foreign object at store key %q (stamp %s)", key, stamp)
		return ctrl.Result{RequeueAfter: jittered(obj.GetInterval())}, nil
	}

	wasReady := conditions.IsTrue(obj, fluxmeta.ReadyCondition)

	// Drift: the key addresses the intent, not the bytes, so the same key can
	// hold different content over time. A change we caused (a run finished
	// since the last verification) re-baselines silently; a change with no run
	// of ours in between means something else wrote to our key.
	generatedSinceLastVerify := obj.Status.GeneratorSucceededAt != nil || obj.Status.GeneratorRef != nil
	if obs.Digest != "" && obj.Status.Digest != "" && obs.Digest != obj.Status.Digest && !generatedSinceLastVerify {
		r.recordDrift(obj, class, key, obj.Status.Digest, obs.Digest)
		// An observe-only Artifact reports the drift but never acts on it: for
		// a sensor, a Regenerate class policy degrades to Warn.
		if class.DriftPolicy() == artifactsv1.DriftPolicyRegenerate && !obj.ObserveOnly() {
			// Treat as missing so the normal generator path restores it.
			obj.Status.Digest = ""
			return r.reconcileMissing(ctx, obj, class, in, now)
		}
	} else if obs.Digest != "" && obs.Digest != obj.Status.Digest {
		// Expected change (or first observation): this is the new baseline.
		conditions.Delete(obj, artifactsv1.ArtifactDriftedCondition)
	}

	t := metav1.NewTime(now)
	obj.Status.Digest = obs.Digest
	obj.Status.LastVerifiedTime = &t
	obj.Status.FailedAttempts = 0
	obj.Status.LastFailureTime = nil
	obj.Status.LastFailureMessage = ""
	obj.Status.GeneratorRef = nil
	obj.Status.GeneratorSucceededAt = nil
	obj.Status.State = artifactsv1.StateReady
	conditions.Delete(obj, fluxmeta.ReconcilingCondition)
	conditions.Delete(obj, fluxmeta.StalledCondition)
	conditions.MarkTrue(obj, artifactsv1.ArtifactInStoreCondition, artifactsv1.ReasonArtifactAvailable,
		"artifact present at %q", key)
	conditions.MarkTrue(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonArtifactAvailable,
		"artifact present at %q", key)
	if !wasReady {
		r.Recorder.Eventf(obj, corev1.EventTypeNormal, "ArtifactAvailable",
			"artifact observed at %q (%s)", key, obs.Digest)
	}
	return ctrl.Result{RequeueAfter: jittered(obj.GetInterval())}, nil
}

// reconcileMissing drives the generator machinery for an absent artifact.
func (r *ArtifactReconciler) reconcileMissing(ctx context.Context, obj *artifactsv1.Artifact, class *artifactsv1.ArtifactClass, in generator.Input, now time.Time) (ctrl.Result, error) {
	if conditions.IsTrue(obj, fluxmeta.ReadyCondition) {
		r.Recorder.Eventf(obj, corev1.EventTypeWarning, "ArtifactMissing",
			"artifact disappeared from store key %q; re-triggering generator", in.Key)
	}
	conditions.MarkFalse(obj, artifactsv1.ArtifactInStoreCondition, artifactsv1.ReasonArtifactMissing,
		"no artifact at %q", in.Key)
	obj.Status.Digest = ""

	// A class without a generator can only back observe-only Artifacts. A
	// Full-policy Artifact pointed at one is a configuration error, surfaced
	// as Stalled — but the store keeps being watched, so an externally
	// produced object still clears it. Checked before the in-flight branch so
	// a class edited to drop its generator mid-run stalls instead of
	// dereferencing nil in evaluateRun.
	if class.Spec.Generator == nil {
		// A run left over from when the class still had a generator is
		// cancelled and forgotten: without the class's expressions it can
		// never be evaluated, and its progress deadline is gone with them —
		// left alone it would leak until the Artifact itself is deleted, or
		// worse, be judged later by expressions from a different template.
		if ref := obj.Status.GeneratorRef; ref != nil {
			r.deleteAbandonedRun(ctx, obj, ref, fmt.Sprintf("class %q no longer defines a generator", class.Name))
			obj.Status.GeneratorRef = nil
			obj.Status.GeneratorSucceededAt = nil
			conditions.Delete(obj, artifactsv1.GeneratorSucceededCondition)
		}
		obj.Status.State = artifactsv1.StateDegraded
		markStalled(obj, artifactsv1.ReasonGeneratorNotConfigured,
			"class %q defines no generator; set managementPolicy: Observe to watch without generating", class.Name)
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonGeneratorNotConfigured,
			"no artifact at %q and class %q defines no generator", in.Key, class.Name)
		return ctrl.Result{RequeueAfter: jittered(obj.GetInterval())}, nil
	}

	maxAttempts, initialDelay, maxDelay := class.EffectiveBackoff()

	// Failure budget exhausted: stay Stalled, but keep observing the store on
	// a slow cadence so an externally restored artifact clears the state.
	if obj.Status.FailedAttempts >= maxAttempts {
		obj.Status.State = artifactsv1.StateDegraded
		markStalled(obj, artifactsv1.ReasonFailureBudgetExhausted,
			"%d consecutive generator failures (budget %d); set the %q annotation to retry",
			obj.Status.FailedAttempts, maxAttempts, artifactsv1.RetryAnnotation)
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonFailureBudgetExhausted,
			"degraded after %d consecutive generator failures", obj.Status.FailedAttempts)
		return ctrl.Result{RequeueAfter: jittered(10 * obj.GetInterval())}, nil
	}

	// A run is in flight: evaluate it.
	if ref := obj.Status.GeneratorRef; ref != nil {
		run := &unstructured.Unstructured{}
		run.SetAPIVersion(ref.APIVersion)
		run.SetKind(ref.Kind)
		err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, run)
		switch {
		case apierrors.IsNotFound(err):
			// The run vanished (manual delete); forget it and recreate.
			obj.Status.GeneratorRef = nil
			obj.Status.GeneratorSucceededAt = nil
			return ctrl.Result{RequeueAfter: time.Second}, nil
		case err != nil:
			return ctrl.Result{}, err
		}
		return r.evaluateRun(ctx, obj, class, run, now, maxAttempts, initialDelay, maxDelay)
	}

	// Backoff gate before a new attempt.
	if lf := obj.Status.LastFailureTime; lf != nil && obj.Status.FailedAttempts > 0 {
		delay := backoffDelay(obj.Status.FailedAttempts, initialDelay, maxDelay)
		if next := lf.Add(delay); now.Before(next) {
			obj.Status.State = artifactsv1.StateGenerating
			markReconciling(obj, artifactsv1.ReasonBackoffPending,
				"next generator attempt at %s", next.Format(time.RFC3339))
			conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonBackoffPending,
				"in backoff after %d failure(s)", obj.Status.FailedAttempts)
			return ctrl.Result{RequeueAfter: next.Sub(now) + time.Second}, nil
		}
	}

	return r.createRun(ctx, obj, class, in, now)
}

func (r *ArtifactReconciler) createRun(ctx context.Context, obj *artifactsv1.Artifact, class *artifactsv1.ArtifactClass, in generator.Input, _ time.Time) (ctrl.Result, error) {
	attempt := obj.Status.Attempts + 1
	in.Attempt = attempt
	run, err := generator.RenderTemplate(class.Spec.Generator.Template.Raw, in)
	if err != nil {
		markStalled(obj, artifactsv1.ReasonTemplateError, "%s", err)
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonTemplateError, "%s", err)
		return ctrl.Result{}, nil // a class edit retriggers via the class watch
	}
	name := runName(obj, in.SpecHash, attempt)
	run.SetName(name)
	run.SetNamespace(obj.Namespace)
	labels := run.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["artifacts.kargops.dev/artifact"] = obj.Name
	labels["artifacts.kargops.dev/spec-hash"] = hash.Short(in.SpecHash, 16)
	run.SetLabels(labels)
	if err := controllerutil.SetControllerReference(obj, run, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	gvk := run.GroupVersionKind()
	if err := r.ensureGeneratorWatch(gvk); err != nil {
		logf.FromContext(ctx).Error(err, "generator watch failed; relying on periodic requeue", "gvk", gvk.String())
	}

	if err := r.Create(ctx, run); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(gvk)
			if gerr := r.Get(ctx, types.NamespacedName{Namespace: obj.Namespace, Name: name}, existing); gerr == nil && metav1.IsControlledBy(existing, obj) {
				// Crash-recovery: we created it but lost the status update.
				obj.Status.Attempts = attempt
				obj.Status.GeneratorRef = generatorRefFor(existing)
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonGeneratorFailed,
				"generator %q already exists and is not owned by this Artifact", name)
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		r.Recorder.Eventf(obj, corev1.EventTypeWarning, "GeneratorCreateFailed", "%s", err)
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonGeneratorFailed,
			"create generator: %s", err)
		return ctrl.Result{RequeueAfter: jittered(time.Minute)}, nil
	}

	obj.Status.Attempts = attempt
	obj.Status.GeneratorRef = generatorRefFor(run)
	obj.Status.GeneratorSucceededAt = nil
	obj.Status.State = artifactsv1.StateGenerating
	markReconciling(obj, artifactsv1.ReasonGenerating,
		"generator %s %q created (attempt %d)", gvk.Kind, name, attempt)
	conditions.MarkUnknown(obj, artifactsv1.GeneratorSucceededCondition, artifactsv1.ReasonGenerating,
		"waiting for %s %q", gvk.Kind, name)
	conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonGenerating,
		"producing artifact (attempt %d)", attempt)
	r.Recorder.Eventf(obj, corev1.EventTypeNormal, "GeneratorCreated",
		"created %s %q (attempt %d)", gvk.Kind, name, attempt)
	return ctrl.Result{RequeueAfter: jittered(30 * time.Second)}, nil
}

func (r *ArtifactReconciler) evaluateRun(ctx context.Context, obj *artifactsv1.Artifact, class *artifactsv1.ArtifactClass, run *unstructured.Unstructured, now time.Time, maxAttempts int32, initialDelay, maxDelay time.Duration) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	failed, ferr := r.Eval.EvalBool(class.Spec.Generator.FailedWhen, run.Object)
	if ferr != nil {
		log.V(1).Info("failedWhen not matched", "error", ferr.Error())
	}
	if failed {
		msg := fmt.Sprintf("generator %s %q reported failure", run.GetKind(), run.GetName())
		return r.recordAttemptFailure(obj, artifactsv1.ReasonGeneratorFailed, msg, now, maxAttempts, initialDelay, maxDelay)
	}

	succeeded, serr := r.Eval.EvalBool(class.Spec.Generator.SucceededWhen, run.Object)
	if serr != nil {
		log.V(1).Info("succeededWhen not matched", "error", serr.Error())
	}
	if succeeded {
		if obj.Status.GeneratorSucceededAt == nil {
			t := metav1.NewTime(now)
			obj.Status.GeneratorSucceededAt = &t
			conditions.MarkTrue(obj, artifactsv1.GeneratorSucceededCondition, artifactsv1.ReasonGeneratorSucceeded,
				"generator %s %q succeeded", run.GetKind(), run.GetName())
			r.Recorder.Eventf(obj, corev1.EventTypeNormal, "GeneratorSucceeded",
				"generator %s %q succeeded; verifying artifact at %q", run.GetKind(), run.GetName(), obj.Status.Key)
		}
		grace := class.GracePeriod()
		elapsed := now.Sub(obj.Status.GeneratorSucceededAt.Time)
		if elapsed < grace {
			obj.Status.State = artifactsv1.StateAwaitingArtifact
			markReconciling(obj, artifactsv1.ReasonAwaitingArtifact,
				"generator succeeded %s ago; waiting up to %s for artifact at %q",
				elapsed.Round(time.Second), grace, obj.Status.Key)
			conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonAwaitingArtifact,
				"waiting for artifact to appear in store")
			requeue := 10 * time.Second
			if rem := grace - elapsed; rem < requeue {
				requeue = rem + time.Second
			}
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		msg := fmt.Sprintf(
			"generator succeeded but no matching artifact appeared at %q within %s; check that the generator uploads to the expected key and stamps %q",
			obj.Status.Key, grace, class.StampMetadataKey())
		return r.recordAttemptFailure(obj, artifactsv1.ReasonSucceededWithoutArtifact, msg, now, maxAttempts, initialDelay, maxDelay)
	}

	// Neither succeeded nor failed. Whether that means "working" or "reporting
	// something this class never anticipated" is only answerable when the
	// class closes the vocabulary with inProgressWhen.
	reason, detail := artifactsv1.ReasonGenerating, "in progress"
	// Only after the run has had time to report. A just-created object has
	// published nothing yet, which is a normal transient — not a state the
	// class failed to describe. Judging on elapsed time rather than on the
	// presence of a status field keeps this true for classes that interpret
	// some other part of the object.
	settled := now.Sub(runStartTime(run, obj)) > unrecognizedGracePeriod
	if expr := class.Spec.Generator.InProgressWhen; expr != "" && settled {
		inProgress, perr := r.Eval.EvalBool(expr, run.Object)
		if perr != nil {
			log.V(1).Info("inProgressWhen not matched", "error", perr.Error())
		}
		if !inProgress {
			// Matched none of the three: either the run reports a state the
			// class does not describe, or an expression is wrong. Both are
			// worth saying out loud rather than waiting on.
			reason = artifactsv1.ReasonStatusUnrecognized
			detail = fmt.Sprintf("status matches none of succeededWhen/failedWhen/inProgressWhen: %s",
				summarizeStatus(run))
		}
	}

	// A run can look healthy at the object level while its execution is
	// wedged — a pod stuck Pending on a missing secret leaves a Job at
	// active:1 with no conditions. Only elapsed time catches that.
	if dl := class.ProgressDeadline(); dl > 0 {
		started := runStartTime(run, obj)
		if elapsed := now.Sub(started); elapsed > dl {
			msg := fmt.Sprintf("generator %s %q made no terminal progress within %s (%s)",
				run.GetKind(), run.GetName(), dl, detail)
			// Delete the run so the next attempt starts clean rather than
			// racing a wedged one that still holds the deterministic name.
			if err := r.Delete(ctx, run, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				logf.FromContext(ctx).Error(err, "deleting stalled generator run", "run", run.GetName())
			}
			return r.recordAttemptFailure(obj, artifactsv1.ReasonProgressDeadlineExceeded, msg, now, maxAttempts, initialDelay, maxDelay)
		}
	}

	obj.Status.State = artifactsv1.StateGenerating
	markReconciling(obj, reason, "generator %s %q: %s", run.GetKind(), run.GetName(), detail)
	conditions.MarkFalse(obj, fluxmeta.ReadyCondition, reason,
		"generator %s (attempt %d)", detail, obj.Status.Attempts)
	return ctrl.Result{RequeueAfter: jittered(30 * time.Second)}, nil
}

// runStartTime is when the current run began, for the progress deadline. The
// run's own creation timestamp is authoritative; the Artifact's record of the
// attempt is the fallback for objects that report no creation time.
func runStartTime(run *unstructured.Unstructured, obj *artifactsv1.Artifact) time.Time {
	if ts := run.GetCreationTimestamp(); !ts.IsZero() {
		return ts.Time
	}
	if lf := obj.Status.LastFailureTime; lf != nil {
		return lf.Time
	}
	return obj.CreationTimestamp.Time
}

// summarizeStatus renders a short, bounded view of the run's status for
// diagnostics, so an unrecognized state names what it actually saw.
func summarizeStatus(run *unstructured.Unstructured) string {
	status, ok := run.Object["status"].(map[string]interface{})
	if !ok || len(status) == 0 {
		return "status is empty"
	}
	keys := make([]string, 0, len(status))
	for k := range status {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := status[k].(type) {
		case string, bool, int64, float64:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		default:
			parts = append(parts, k)
		}
	}
	out := strings.Join(parts, " ")
	if len(out) > 160 {
		out = out[:157] + "..."
	}
	return out
}

func (r *ArtifactReconciler) recordAttemptFailure(obj *artifactsv1.Artifact, reason, msg string, now time.Time, maxAttempts int32, initialDelay, maxDelay time.Duration) (ctrl.Result, error) {
	obj.Status.FailedAttempts++
	t := metav1.NewTime(now)
	obj.Status.LastFailureTime = &t
	obj.Status.LastFailureMessage = msg
	obj.Status.GeneratorRef = nil
	obj.Status.GeneratorSucceededAt = nil
	conditions.MarkFalse(obj, artifactsv1.GeneratorSucceededCondition, reason, "%s", msg)
	r.Recorder.Event(obj, corev1.EventTypeWarning, reason, msg)

	if obj.Status.FailedAttempts >= maxAttempts {
		obj.Status.State = artifactsv1.StateDegraded
		markStalled(obj, artifactsv1.ReasonFailureBudgetExhausted,
			"%d consecutive generator failures (budget %d); set the %q annotation to retry",
			obj.Status.FailedAttempts, maxAttempts, artifactsv1.RetryAnnotation)
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonFailureBudgetExhausted,
			"degraded after %d consecutive generator failures: %s", obj.Status.FailedAttempts, msg)
		r.Recorder.Eventf(obj, corev1.EventTypeWarning, "Degraded",
			"failure budget exhausted (%d/%d)", obj.Status.FailedAttempts, maxAttempts)
		return ctrl.Result{RequeueAfter: jittered(10 * obj.GetInterval())}, nil
	}

	obj.Status.State = artifactsv1.StateGenerating
	markReconciling(obj, artifactsv1.ReasonBackoffPending, "attempt %d failed; backing off", obj.Status.Attempts)
	conditions.MarkFalse(obj, fluxmeta.ReadyCondition, reason, "%s", msg)
	return ctrl.Result{RequeueAfter: backoffDelay(obj.Status.FailedAttempts, initialDelay, maxDelay)}, nil
}

// reconcileMissingObserved is the missing-artifact path for observe-only
// Artifacts: report absence and keep watching. Nothing is generated and
// nothing is deleted from the store — Ready simply stays False until an
// external producer fills the key.
func (r *ArtifactReconciler) reconcileMissingObserved(ctx context.Context, obj *artifactsv1.Artifact, in generator.Input) (ctrl.Result, error) {
	if conditions.IsTrue(obj, fluxmeta.ReadyCondition) {
		r.Recorder.Eventf(obj, corev1.EventTypeWarning, "ArtifactMissing",
			"artifact disappeared from store key %q; managementPolicy is Observe, not regenerating", in.Key)
	}
	// A run created before the policy flipped to Observe is cancelled: a
	// sensor must not have this controller's machinery writing to its key.
	if ref := obj.Status.GeneratorRef; ref != nil {
		r.deleteAbandonedRun(ctx, obj, ref, "managementPolicy is Observe")
	}
	// Generator bookkeeping is meaningless for a sensor. Clearing the failure
	// budget also means a later Observe→Full flip starts fresh instead of
	// stalling on a budget exhausted in a previous life; status.attempts is
	// deliberately kept — it is a lifetime counter that keeps run names from
	// colliding with runs created before the flip.
	obj.Status.GeneratorRef = nil
	obj.Status.GeneratorSucceededAt = nil
	obj.Status.FailedAttempts = 0
	obj.Status.LastFailureTime = nil
	obj.Status.LastFailureMessage = ""
	conditions.Delete(obj, artifactsv1.GeneratorSucceededCondition)
	obj.Status.Digest = ""
	obj.Status.State = artifactsv1.StateMissing
	// Neither Reconciling nor Stalled: nothing is in progress and nothing is
	// wrong — the artifact is just not there yet, and producing it is
	// somebody else's job.
	conditions.Delete(obj, fluxmeta.ReconcilingCondition)
	conditions.Delete(obj, fluxmeta.StalledCondition)
	conditions.MarkFalse(obj, artifactsv1.ArtifactInStoreCondition, artifactsv1.ReasonArtifactMissing,
		"no artifact at %q", in.Key)
	conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonArtifactMissing,
		"no artifact at %q; waiting for an external producer (managementPolicy: Observe)", in.Key)
	return ctrl.Result{RequeueAfter: jittered(obj.GetInterval())}, nil
}

// deleteAbandonedRun best-effort deletes an owned generator run the Artifact
// can no longer make use of. On failure the run is only logged: the owner
// reference still garbage-collects it when the Artifact goes away.
func (r *ArtifactReconciler) deleteAbandonedRun(ctx context.Context, obj *artifactsv1.Artifact, ref *artifactsv1.GeneratorReference, why string) {
	run := &unstructured.Unstructured{}
	run.SetAPIVersion(ref.APIVersion)
	run.SetKind(ref.Kind)
	run.SetName(ref.Name)
	run.SetNamespace(ref.Namespace)
	err := r.Delete(ctx, run, client.PropagationPolicy(metav1.DeletePropagationBackground))
	switch {
	case err == nil:
		r.Recorder.Eventf(obj, corev1.EventTypeNormal, "GeneratorRunCancelled",
			"deleted generator run %s %q: %s", ref.Kind, ref.Name, why)
	case !apierrors.IsNotFound(err):
		logf.FromContext(ctx).Error(err, "deleting abandoned generator run", "run", ref.Name)
	}
}

// reconcileExpired implements deleteAfter: delete the store object (if it is
// ours), park the CR as Expired, and only wake up again for a pending ttl.
func (r *ArtifactReconciler) reconcileExpired(ctx context.Context, obj *artifactsv1.Artifact, class *artifactsv1.ArtifactClass, driver store.Driver, key, specHash string, now time.Time) (ctrl.Result, error) {
	obs, err := driver.Observe(ctx, key)
	if err != nil {
		conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonStoreUnavailable, "%s", err)
		return ctrl.Result{RequeueAfter: jittered(time.Minute)}, nil
	}
	if obs.Exists {
		if stamp := obs.Metadata[class.StampMetadataKey()]; stamp == "" || stamp == specHash {
			if err := driver.Delete(ctx, key); err != nil {
				conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonStoreUnavailable,
					"deleteAfter cleanup: %s", err)
				return ctrl.Result{RequeueAfter: jittered(time.Minute)}, nil
			}
			r.Recorder.Eventf(obj, corev1.EventTypeNormal, "ArtifactDeleted",
				"deleteAfter reached; deleted artifact at %q", key)
		} else {
			r.Recorder.Eventf(obj, corev1.EventTypeWarning, "KeyConflict",
				"deleteAfter reached but object at %q is stamped %s; leaving it in place", key, stamp)
		}
	}
	obj.Status.State = artifactsv1.StateExpired
	obj.Status.Digest = ""
	obj.Status.GeneratorRef = nil
	obj.Status.GeneratorSucceededAt = nil
	conditions.Delete(obj, fluxmeta.ReconcilingCondition)
	conditions.Delete(obj, fluxmeta.StalledCondition)
	conditions.MarkFalse(obj, artifactsv1.ArtifactInStoreCondition, artifactsv1.ReasonExpired,
		"deleteAfter reached; artifact deleted from store")
	conditions.MarkFalse(obj, fluxmeta.ReadyCondition, artifactsv1.ReasonExpired,
		"deleteAfter %s reached; artifact is no longer reconciled", obj.Spec.DeleteAfter.Duration)

	if ttl := obj.Spec.TTL; ttl != nil && ttl.Duration > 0 {
		if remaining := ttl.Duration - now.Sub(obj.CreationTimestamp.Time); remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining + time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileDelete applies the deletion policy and removes the finalizer.
func (r *ArtifactReconciler) reconcileDelete(ctx context.Context, obj *artifactsv1.Artifact) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(obj, artifactsv1.ArtifactFinalizer) {
		return ctrl.Result{}, nil
	}
	// CRD validation rejects deletionPolicy Delete on observe-only Artifacts;
	// the policy check here is defense in depth for objects admitted before
	// the rule existed.
	if obj.Spec.DeletionPolicy == artifactsv1.DeletionPolicyDelete && obj.Status.Key != "" && !obj.ObserveOnly() {
		class := &artifactsv1.ArtifactClass{}
		err := r.Get(ctx, types.NamespacedName{Name: obj.Spec.ClassRef.Name}, class)
		switch {
		case apierrors.IsNotFound(err):
			r.Recorder.Eventf(obj, corev1.EventTypeWarning, "OrphanedArtifact",
				"deletionPolicy is Delete but ArtifactClass %q is gone; store object at %q is orphaned",
				obj.Spec.ClassRef.Name, obj.Status.Key)
		case err != nil:
			return ctrl.Result{}, err
		default:
			driver, derr := r.Registry.DriverFor(ctx, class)
			if derr != nil {
				return ctrl.Result{}, derr
			}
			obs, oerr := driver.Observe(ctx, obj.Status.Key)
			if oerr != nil {
				return ctrl.Result{}, oerr
			}
			if obs.Exists {
				stamp := obs.Metadata[class.StampMetadataKey()]
				if stamp == "" || stamp == obj.Status.SpecHash {
					if err := driver.Delete(ctx, obj.Status.Key); err != nil {
						return ctrl.Result{}, err
					}
					r.Recorder.Eventf(obj, corev1.EventTypeNormal, "ArtifactDeleted",
						"deleted artifact at %q per deletionPolicy", obj.Status.Key)
				} else {
					r.Recorder.Eventf(obj, corev1.EventTypeWarning, "KeyConflict",
						"object at %q is stamped %s; leaving it in place", obj.Status.Key, stamp)
				}
			}
		}
	}
	controllerutil.RemoveFinalizer(obj, artifactsv1.ArtifactFinalizer)
	return ctrl.Result{}, nil
}

func (r *ArtifactReconciler) patchOpts() []patch.Option {
	return []patch.Option{
		patch.WithOwnedConditions{Conditions: []string{
			fluxmeta.ReadyCondition,
			fluxmeta.ReconcilingCondition,
			fluxmeta.StalledCondition,
			artifactsv1.ArtifactInStoreCondition,
			artifactsv1.GeneratorSucceededCondition,
		}},
		patch.WithFieldOwner(r.FieldOwner),
	}
}

// --- helpers ---

func generatorRefFor(u *unstructured.Unstructured) *artifactsv1.GeneratorReference {
	return &artifactsv1.GeneratorReference{
		APIVersion: u.GetAPIVersion(),
		Kind:       u.GetKind(),
		Name:       u.GetName(),
		Namespace:  u.GetNamespace(),
	}
}

func runName(obj *artifactsv1.Artifact, specHash string, attempt int32) string {
	base := obj.Name
	if len(base) > 40 {
		base = base[:40]
	}
	return fmt.Sprintf("%s-%s-r%d", base, hash.Short(specHash, 8), attempt)
}

func markStalled(obj conditions.Setter, reason, msgFmt string, args ...interface{}) {
	conditions.Delete(obj, fluxmeta.ReconcilingCondition)
	conditions.MarkTrue(obj, fluxmeta.StalledCondition, reason, msgFmt, args...)
}

func markReconciling(obj conditions.Setter, reason, msgFmt string, args ...interface{}) {
	conditions.Delete(obj, fluxmeta.StalledCondition)
	conditions.MarkTrue(obj, fluxmeta.ReconcilingCondition, reason, msgFmt, args...)
}

// backoffDelay is initial*2^(failed-1) capped at max (cert-manager style).
func backoffDelay(failed int32, initial, max time.Duration) time.Duration {
	d := initial
	for i := int32(1); i < failed; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

// jittered spreads requeues by ±10% to avoid thundering herds against the
// store API (the same convention Flux applies to source intervals).
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// recordDrift reports content that changed at the key without this controller
// having generated it. The store's own digest is what makes this detectable:
// it is computed by the store, so the writer cannot forge it.
func (r *ArtifactReconciler) recordDrift(obj *artifactsv1.Artifact, class *artifactsv1.ArtifactClass, key, was, now string) {
	if class.DriftPolicy() == artifactsv1.DriftPolicyIgnore {
		return
	}
	msg := fmt.Sprintf("content at %q changed from %s to %s with no generator run of ours in between", key, was, now)
	conditions.MarkTrue(obj, artifactsv1.ArtifactDriftedCondition, artifactsv1.ReasonContentDrifted, "%s", msg)
	r.Recorder.Event(obj, corev1.EventTypeWarning, artifactsv1.ReasonContentDrifted, msg)
}
