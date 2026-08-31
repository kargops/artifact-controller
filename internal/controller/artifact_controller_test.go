package controller

import (
	"fmt"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/hash"
)

// The tests use a ConfigMap as the generator kind: it needs no status
// subresource (the test "completes" a run by setting data.result), which
// exercises the exact same generic template/CEL/ownership machinery as an
// Argo Workflow or Tekton PipelineRun.
const cmTemplate = `{"apiVersion":"v1","kind":"ConfigMap","data":{"specHash":"{{ .SpecHash }}","key":"{{ .Key }}","attempt":"{{ .Attempt }}"}}`

func newClass(name string, maxAttempts int32, grace, backoff time.Duration) *artifactsv1.ArtifactClass {
	return &artifactsv1.ArtifactClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: artifactsv1.ArtifactClassSpec{
			Store: artifactsv1.StoreSpec{
				Driver:      "fake",
				KeyTemplate: "test/{{ .SpecHash }}",
				Fake:        &artifactsv1.FakeStoreSpec{},
			},
			Generator: artifactsv1.GeneratorSpec{
				Template:      runtime.RawExtension{Raw: []byte(cmTemplate)},
				SucceededWhen: `'result' in object.data && object.data['result'] == 'ok'`,
				FailedWhen:    `'result' in object.data && object.data['result'] == 'fail'`,
			},
			Backoff: &artifactsv1.BackoffSpec{
				MaxAttempts:  maxAttempts,
				InitialDelay: metav1.Duration{Duration: backoff},
				MaxDelay:     metav1.Duration{Duration: backoff},
			},
			VerificationGracePeriod: metav1.Duration{Duration: grace},
		},
	}
}

func newArtifact(name, class string, mutate ...func(*artifactsv1.Artifact)) *artifactsv1.Artifact {
	a := &artifactsv1.Artifact{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: artifactsv1.ArtifactSpec{
			ClassRef: fluxmeta.LocalObjectReference{Name: class},
			Identity: map[string]string{"test": name},
			Interval: metav1.Duration{Duration: time.Second},
		},
	}
	for _, m := range mutate {
		m(a)
	}
	return a
}

func identityHash(name string) string {
	return hash.Canonical(map[string]string{"test": name})
}

func storeKey(name string) string {
	return "test/" + identityHash(name)
}

func stamped(name string) map[string]string {
	return map[string]string{artifactsv1.DefaultStampMetadataKey: identityHash(name)}
}

func cmName(artifact, name string, attempt int) string {
	return fmt.Sprintf("%s-%s-r%d", artifact, hash.Short(identityHash(name), 8), attempt)
}

func getArtifact(g Gomega, name string) *artifactsv1.Artifact {
	a := &artifactsv1.Artifact{}
	g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: name}, a)).To(Succeed())
	return a
}

func setCMResult(g Gomega, name, val string) {
	g.Eventually(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: name}, cm)).To(Succeed())
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["result"] = val
		g.Expect(k8sClient.Update(testCtx, cm)).To(Succeed())
	}).Should(Succeed())
}

func condReason(a *artifactsv1.Artifact, condType string) string {
	if c := apimeta.FindStatusCondition(a.Status.Conditions, condType); c != nil {
		return c.Reason
	}
	return ""
}

func TestGenerateVerifyRemediate(t *testing.T) {
	g := NewWithT(t)
	class := newClass("happy", 3, 30*time.Second, time.Second)
	g.Expect(k8sClient.Create(testCtx, class)).To(Succeed())
	art := newArtifact("art1", "happy")
	g.Expect(k8sClient.Create(testCtx, art)).To(Succeed())

	// Generator run r1 is created from the rendered template.
	run1 := cmName("art1", "art1", 1)
	g.Eventually(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: run1}, cm)).To(Succeed())
		g.Expect(cm.Data["specHash"]).To(Equal(identityHash("art1")))
		g.Expect(cm.Data["key"]).To(Equal(storeKey("art1")))
		g.Expect(metav1.IsControlledBy(cm, getArtifact(g, "art1"))).To(BeTrue())
	}).Should(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art1")
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateGenerating))
		g.Expect(a.Status.GeneratorRef).NotTo(BeNil())
		g.Expect(a.Status.Key).To(Equal(storeKey("art1")))
	}).Should(Succeed())

	// Generator succeeds -> grace window while the artifact is not in store.
	setCMResult(g, run1, "ok")
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art1")
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateAwaitingArtifact))
		g.Expect(a.Status.GeneratorSucceededAt).NotTo(BeNil())
	}).Should(Succeed())

	// Artifact lands in the store with the right stamp -> Ready.
	fakeStore.Put(storeKey("art1"), "etag:one", stamped("art1"))
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art1")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateReady))
		g.Expect(a.Status.Digest).To(Equal("etag:one"))
		g.Expect(a.Status.FailedAttempts).To(BeZero())
	}).Should(Succeed())

	// Artifact vanishes from the store -> remediation retriggers a new run.
	g.Expect(fakeStore.Delete(testCtx, storeKey("art1"))).To(Succeed())
	run2 := cmName("art1", "art1", 2)
	g.Eventually(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: run2}, cm)).To(Succeed())
	}).Should(Succeed())

	setCMResult(g, run2, "ok")
	fakeStore.Put(storeKey("art1"), "etag:two", stamped("art1"))
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art1")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
		g.Expect(a.Status.Digest).To(Equal("etag:two"))
	}).Should(Succeed())
}

func TestFailureBudgetDegradedAndRetryAnnotation(t *testing.T) {
	g := NewWithT(t)
	class := newClass("flaky", 2, 2*time.Second, 200*time.Millisecond)
	g.Expect(k8sClient.Create(testCtx, class)).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("art2", "flaky"))).To(Succeed())

	setCMResult(g, cmName("art2", "art2", 1), "fail")
	g.Eventually(func(g Gomega) {
		g.Expect(getArtifact(g, "art2").Status.FailedAttempts).To(BeNumerically(">=", 1))
	}).Should(Succeed())

	setCMResult(g, cmName("art2", "art2", 2), "fail")
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art2")
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateDegraded))
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.StalledCondition)).To(BeTrue())
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonFailureBudgetExhausted))
	}).Should(Succeed())

	// Degraded means no further attempts.
	g.Consistently(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: cmName("art2", "art2", 3)}, cm)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}).WithTimeout(2 * time.Second).Should(Succeed())

	// The retry annotation resets the budget and produces attempt 3.
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art2")
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		a.Annotations[artifactsv1.RetryAnnotation] = "1"
		g.Expect(k8sClient.Update(testCtx, a)).To(Succeed())
	}).Should(Succeed())

	run3 := cmName("art2", "art2", 3)
	g.Eventually(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: run3}, cm)).To(Succeed())
	}).Should(Succeed())

	setCMResult(g, run3, "ok")
	fakeStore.Put(storeKey("art2"), "etag:recovered", stamped("art2"))
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art2")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
		g.Expect(apimeta.FindStatusCondition(a.Status.Conditions, fluxmeta.StalledCondition)).To(BeNil())
	}).Should(Succeed())
}

func TestPreExistingArtifactShortCircuitsGenerator(t *testing.T) {
	g := NewWithT(t)
	fakeStore.Put(storeKey("art3"), "etag:pre", stamped("art3"))
	g.Expect(k8sClient.Create(testCtx, newArtifact("art3", "happy"))).To(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art3")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
	}).Should(Succeed())

	g.Consistently(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: cmName("art3", "art3", 1)}, cm)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}).WithTimeout(time.Second).Should(Succeed())
}

func TestKeyConflictRefusesForeignObject(t *testing.T) {
	g := NewWithT(t)
	fakeStore.Put(storeKey("art4"), "etag:foreign", map[string]string{artifactsv1.DefaultStampMetadataKey: "sha256:someoneelse"})
	g.Expect(k8sClient.Create(testCtx, newArtifact("art4", "happy"))).To(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art4")
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateKeyConflict))
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonKeyConflict))
	}).Should(Succeed())

	// It must not trigger a generator or delete the foreign object.
	g.Consistently(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: cmName("art4", "art4", 1)}, cm)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		obs, oerr := fakeStore.Observe(testCtx, storeKey("art4"))
		g.Expect(oerr).NotTo(HaveOccurred())
		g.Expect(obs.Exists).To(BeTrue())
	}).WithTimeout(time.Second).Should(Succeed())
}

func TestDeletionPolicyDelete(t *testing.T) {
	g := NewWithT(t)
	fakeStore.Put(storeKey("art5"), "etag:five", stamped("art5"))
	art := newArtifact("art5", "happy", func(a *artifactsv1.Artifact) {
		a.Spec.DeletionPolicy = artifactsv1.DeletionPolicyDelete
	})
	g.Expect(k8sClient.Create(testCtx, art)).To(Succeed())

	g.Eventually(func(g Gomega) {
		g.Expect(apimeta.IsStatusConditionTrue(getArtifact(g, "art5").Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
	}).Should(Succeed())

	g.Expect(k8sClient.Delete(testCtx, art)).To(Succeed())
	g.Eventually(func(g Gomega) {
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: "art5"}, &artifactsv1.Artifact{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		obs, oerr := fakeStore.Observe(testCtx, storeKey("art5"))
		g.Expect(oerr).NotTo(HaveOccurred())
		g.Expect(obs.Exists).To(BeFalse())
	}).Should(Succeed())
}

func TestTTLDeletesTheCRAndOrphansTheArtifact(t *testing.T) {
	g := NewWithT(t)
	fakeStore.Put(storeKey("art6"), "etag:six", stamped("art6"))
	art := newArtifact("art6", "happy", func(a *artifactsv1.Artifact) {
		a.Spec.TTL = &metav1.Duration{Duration: 2 * time.Second}
	})
	g.Expect(k8sClient.Create(testCtx, art)).To(Succeed())

	g.Eventually(func(g Gomega) {
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: "art6"}, &artifactsv1.Artifact{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}).Should(Succeed())

	// Default policy Orphan: the store object survives the CR.
	obs, err := fakeStore.Observe(testCtx, storeKey("art6"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(obs.Exists).To(BeTrue())
}

// A class naming a driver this controller has not registered (for example
// "fake" in a production binary started without --enable-fake-store) must fail
// loudly and, critically, must never run a generator — otherwise a
// misconfigured class silently burns pipeline capacity.
func TestUnregisteredDriverFailsWithoutRunningAGenerator(t *testing.T) {
	g := NewWithT(t)
	class := newClass("unregistered", 3, 30*time.Second, time.Second)
	class.Spec.Store.Driver = "s3" // valid in the schema, not registered in this suite
	class.Spec.Store.Fake = nil
	class.Spec.Store.S3 = &artifactsv1.S3StoreSpec{Bucket: "nope"}
	g.Expect(k8sClient.Create(testCtx, class)).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("art8", "unregistered"))).To(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art8")
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonStoreUnavailable))
		c := apimeta.FindStatusCondition(a.Status.Conditions, fluxmeta.ReadyCondition)
		g.Expect(c.Message).To(ContainSubstring("no store driver registered"))
		g.Expect(c.Message).To(ContainSubstring("available: fake"))
	}).Should(Succeed())

	g.Consistently(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: cmName("art8", "art8", 1)}, cm)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		g.Expect(getArtifact(g, "art8").Status.Attempts).To(BeZero())
	}).WithTimeout(2 * time.Second).Should(Succeed())
}

// A run reporting something the class never described must say so, rather
// than being indistinguishable from a healthy build. Without inProgressWhen
// the same run is treated as progressing — that is the open vocabulary the
// field closes.
func TestUnrecognizedGeneratorStatusIsSurfaced(t *testing.T) {
	g := NewWithT(t)
	// A newly created run has reported nothing yet, which is not the same as
	// reporting something unrecognized; the controller waits this out first.
	// Shortened so the test does not have to.
	restore := unrecognizedGracePeriod
	unrecognizedGracePeriod = time.Second
	defer func() { unrecognizedGracePeriod = restore }()

	class := newClass("closed-vocab", 3, 30*time.Second, time.Second)
	class.Spec.Generator.InProgressWhen = `'result' in object.data && object.data['result'] == 'running'`
	g.Expect(k8sClient.Create(testCtx, class)).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("art9", "closed-vocab"))).To(Succeed())

	run := cmName("art9", "art9", 1)
	setCMResult(g, run, "running")
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art9")
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonGenerating))
	}).Should(Succeed())

	// A state the class does not describe: not succeeded, not failed, not in progress.
	setCMResult(g, run, "wat")
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art9")
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonStatusUnrecognized))
		c := apimeta.FindStatusCondition(a.Status.Conditions, fluxmeta.ReadyCondition)
		g.Expect(c.Message).To(ContainSubstring("matches none of"))
		// It must still be waiting, not failed: unrecognized is a diagnosis.
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateGenerating))
	}).Should(Succeed())
}

// A run whose object looks alive but never reaches a terminal state must be
// failed on elapsed time — no status expression can see a wedged execution.
func TestProgressDeadlineFailsAStalledRun(t *testing.T) {
	g := NewWithT(t)
	class := newClass("deadline", 3, 30*time.Second, time.Second)
	class.Spec.Generator.ProgressDeadline = &metav1.Duration{Duration: 2 * time.Second}
	g.Expect(k8sClient.Create(testCtx, class)).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("art10", "deadline"))).To(Succeed())

	// Never touch the run: it stays non-terminal forever, like a pod wedged
	// in Pending.
	run1 := cmName("art10", "art10", 1)
	g.Eventually(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: run1}, cm)).To(Succeed())
	}).Should(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art10")
		g.Expect(a.Status.FailedAttempts).To(BeNumerically(">=", 1))
		g.Expect(a.Status.LastFailureMessage).To(ContainSubstring("no terminal progress within"))
	}).Should(Succeed())

	// The stalled run is removed so the next attempt does not race it.
	g.Eventually(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: run1}, cm)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}).Should(Succeed())
}

// The key addresses the intent, not the bytes, so the same key can hold
// different content over time. A change the controller caused is expected; a
// change with no run of ours in between is someone else writing to our key.
func TestDriftIsReportedButNotSelfInflicted(t *testing.T) {
	g := NewWithT(t)
	class := newClass("drifty", 3, 30*time.Second, time.Second)
	g.Expect(k8sClient.Create(testCtx, class)).To(Succeed())

	// Pre-existing artifact: goes Ready without ever running a generator, so
	// there is no run to confuse the "did we cause it?" test.
	fakeStore.Put(storeKey("art11"), "digest-one", stamped("art11"))
	g.Expect(k8sClient.Create(testCtx, newArtifact("art11", "drifty"))).To(Succeed())
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art11")
		g.Expect(a.Status.Digest).To(Equal("digest-one"))
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
	}).Should(Succeed())

	// Someone overwrites the object, keeping the stamp: verification alone
	// cannot see this — only the digest can.
	fakeStore.Put(storeKey("art11"), "digest-two", stamped("art11"))
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art11")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, artifactsv1.ArtifactDriftedCondition)).To(BeTrue())
		c := apimeta.FindStatusCondition(a.Status.Conditions, artifactsv1.ArtifactDriftedCondition)
		g.Expect(c.Message).To(ContainSubstring("digest-one"))
		g.Expect(c.Message).To(ContainSubstring("digest-two"))
		// Warn is the default: the artifact is still usable.
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateReady))
		g.Expect(a.Status.Digest).To(Equal("digest-two"))
	}).Should(Succeed())
}

func TestDeleteAfterRemovesTheArtifactAndParksTheCR(t *testing.T) {
	g := NewWithT(t)
	fakeStore.Put(storeKey("art7"), "etag:seven", stamped("art7"))
	art := newArtifact("art7", "happy", func(a *artifactsv1.Artifact) {
		a.Spec.DeleteAfter = &metav1.Duration{Duration: 2 * time.Second}
	})
	g.Expect(k8sClient.Create(testCtx, art)).To(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "art7")
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateExpired))
		obs, oerr := fakeStore.Observe(testCtx, storeKey("art7"))
		g.Expect(oerr).NotTo(HaveOccurred())
		g.Expect(obs.Exists).To(BeFalse())
	}).Should(Succeed())
}
