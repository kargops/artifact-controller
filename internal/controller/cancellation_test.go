package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
	"github.com/kargops/artifact-controller/internal/store/fake"
)

// A failed cancellation must be retried, not forgotten: the generator ref may
// only be cleared once the delete is confirmed. This drives Reconcile against
// a fake client whose first Delete fails, without the envtest manager, so the
// failure is injected deterministically.
func TestFailedRunCancellationIsRetriedNotForgotten(t *testing.T) {
	g := NewWithT(t)
	s := runtime.NewScheme()
	g.Expect(scheme.AddToScheme(s)).To(Succeed())
	g.Expect(artifactsv1.AddToScheme(s)).To(Succeed())

	art := &artifactsv1.Artifact{
		ObjectMeta: metav1.ObjectMeta{
			Name: "flip", Namespace: "default",
			Finalizers: []string{artifactsv1.ArtifactFinalizer},
		},
		Spec: artifactsv1.ArtifactSpec{
			ClassRef:         fluxmeta.LocalObjectReference{Name: "cancel-class"},
			Identity:         map[string]string{"test": "flip"},
			ManagementPolicy: artifactsv1.ManagementPolicyObserve,
		},
		Status: artifactsv1.ArtifactStatus{
			GeneratorRef: &artifactsv1.GeneratorReference{
				APIVersion: "v1", Kind: "ConfigMap", Name: "flip-run", Namespace: "default",
			},
		},
	}
	class := &artifactsv1.ArtifactClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cancel-class"},
		Spec: artifactsv1.ArtifactClassSpec{
			Store: artifactsv1.StoreSpec{Driver: "fake", KeyTemplate: "t/{{ .SpecHash }}", Fake: &artifactsv1.FakeStoreSpec{}},
		},
	}

	deletesAttempted := 0
	c := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(art, class).
		WithStatusSubresource(&artifactsv1.Artifact{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "ConfigMap" {
					deletesAttempted++
					if deletesAttempted == 1 {
						return fmt.Errorf("simulated transient apiserver failure")
					}
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	reg := store.NewRegistry()
	fake.Register(reg, fake.New())
	r := &ArtifactReconciler{
		Client:   c,
		Scheme:   s,
		Registry: reg,
		Recorder: record.NewFakeRecorder(32),
		Now:      time.Now,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "flip"}}

	// First pass: the delete fails, reconciliation errors, and — critically —
	// the ref survives so the cancellation can be retried.
	_, err := r.Reconcile(context.Background(), req)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cancel generator run"))
	after := &artifactsv1.Artifact{}
	g.Expect(c.Get(context.Background(), req.NamespacedName, after)).To(Succeed())
	g.Expect(after.Status.GeneratorRef).NotTo(BeNil())

	// Second pass: the delete goes through (NotFound counts as confirmed
	// gone) and the bookkeeping is cleared.
	_, err = r.Reconcile(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.Get(context.Background(), req.NamespacedName, after)).To(Succeed())
	g.Expect(after.Status.GeneratorRef).To(BeNil())
	g.Expect(after.Status.State).To(Equal(artifactsv1.StateMissing))
	g.Expect(deletesAttempted).To(Equal(2))
}
