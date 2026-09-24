package controller

// Promotion (two-phase write) coverage: generators upload to the incoming
// key, the controller verifies and promotes to the canonical key, readiness
// gates on the promoted content digest, and content whose provenance is
// disproven is refused rather than adopted.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
	"github.com/kargops/artifact-controller/internal/store/fake"
)

// observeDeleteOnly hides the fake store's Promote, modelling a driver that
// cannot do promotion. Registered as "nexus" in the suite.
type observeDeleteOnly struct{ inner *fake.Store }

func (o observeDeleteOnly) Observe(ctx context.Context, key string) (store.Observation, error) {
	return o.inner.Observe(ctx, key)
}

func (o observeDeleteOnly) Delete(ctx context.Context, key string) error {
	return o.inner.Delete(ctx, key)
}

// promoCMTemplate additionally renders .IncomingKey — the field a promotion
// class's generator must upload to.
const promoCMTemplate = `{"apiVersion":"v1","kind":"ConfigMap","data":{"specHash":"{{ .SpecHash }}","key":"{{ .Key }}","incomingKey":"{{ .IncomingKey }}","attempt":"{{ .Attempt }}"}}`

func newPromotionClass(name string, maxAttempts int32, grace, backoff time.Duration) *artifactsv1.ArtifactClass {
	class := newClass(name, maxAttempts, grace, backoff)
	class.Spec.Store.Promotion = &artifactsv1.PromotionSpec{}
	class.Spec.Generator.Template = runtime.RawExtension{Raw: []byte(promoCMTemplate)}
	return class
}

func incomingKey(name string) string {
	return artifactsv1.DefaultIncomingPrefix + storeKey(name)
}

func contentSHA(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPromotionFlow(t *testing.T) {
	g := NewWithT(t)
	g.Expect(k8sClient.Create(testCtx, newPromotionClass("promo", 3, 30*time.Second, time.Second))).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("promo1", "promo"))).To(Succeed())

	// The run template sees the incoming key.
	run1 := cmName("promo1", "promo1", 1)
	g.Eventually(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		g.Expect(k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: run1}, cm)).To(Succeed())
		g.Expect(cm.Data["incomingKey"]).To(Equal(incomingKey("promo1")))
	}).Should(Succeed())

	// Generator uploads to the incoming key (stamped) and reports success.
	fakeStore.PutContent(incomingKey("promo1"), "payload-one", stamped("promo1"))
	setCMResult(g, run1, "ok")

	// The controller promotes: canonical object appears with both stamps,
	// the incoming object is cleaned up, and the Artifact is Ready with the
	// content digest recorded.
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo1")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
		g.Expect(a.Status.ContentDigest).To(Equal(contentSHA("payload-one")))

		obs, err := fakeStore.Observe(testCtx, storeKey("promo1"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(obs.Exists).To(BeTrue())
		g.Expect(obs.Metadata[artifactsv1.DefaultStampMetadataKey]).To(Equal(identityHash("promo1")))
		g.Expect(obs.Metadata[artifactsv1.DefaultContentDigestKey]).To(Equal(contentSHA("payload-one")))

		iobs, err := fakeStore.Observe(testCtx, incomingKey("promo1"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(iobs.Exists).To(BeFalse())
	}).Should(Succeed())
}

func TestPromotionRefusesMisstampedIncoming(t *testing.T) {
	g := NewWithT(t)
	g.Expect(k8sClient.Create(testCtx, newPromotionClass("promo-strict", 3, 30*time.Second, time.Minute))).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("promo2", "promo-strict"))).To(Succeed())

	// The incoming object carries someone else's stamp (an unstamped object
	// is the same case: promotion requires stamp == spec hash).
	fakeStore.PutContent(incomingKey("promo2"), "not-ours",
		map[string]string{artifactsv1.DefaultStampMetadataKey: "sha256:someoneelse"})
	setCMResult(g, cmName("promo2", "promo2", 1), "ok")

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo2")
		g.Expect(a.Status.FailedAttempts).To(BeNumerically(">=", 1))
		g.Expect(condReason(a, artifactsv1.GeneratorSucceededCondition)).To(Equal(artifactsv1.ReasonIncomingStampMismatch))
	}).Should(Succeed())

	// Nothing was promoted.
	obs, err := fakeStore.Observe(testCtx, storeKey("promo2"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(obs.Exists).To(BeFalse())
}

func TestPromotionSealsPreexistingObject(t *testing.T) {
	g := NewWithT(t)
	// A direct-write-era object: spec stamp only, no content-digest stamp —
	// what the store holds when a class migrates to promotion.
	fakeStore.PutContent(storeKey("promo3"), "legacy-content", stamped("promo3"))
	g.Expect(k8sClient.Create(testCtx, newPromotionClass("promo-migrate", 3, 30*time.Second, time.Second))).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("promo3", "promo-migrate"))).To(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo3")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
		g.Expect(a.Status.ContentDigest).To(Equal(contentSHA("legacy-content")))

		obs, err := fakeStore.Observe(testCtx, storeKey("promo3"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(obs.Metadata[artifactsv1.DefaultContentDigestKey]).To(Equal(contentSHA("legacy-content")))
		g.Expect(obs.Metadata[artifactsv1.DefaultStampMetadataKey]).To(Equal(identityHash("promo3")))
	}).Should(Succeed())

	// Sealed by observation, not rebuilt.
	cm := &corev1.ConfigMap{}
	err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: cmName("promo3", "promo3", 1)}, cm)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestPromotionAdoptsAlreadyPromotedObject(t *testing.T) {
	g := NewWithT(t)
	// Dedup: the key already holds a promoted object (spec stamp + content
	// stamp, both consistent). A new Artifact of the same intent becomes
	// Ready by observation and records the digest it verified.
	md := stamped("promo4")
	md[artifactsv1.DefaultContentDigestKey] = contentSHA("shared-content")
	fakeStore.PutContent(storeKey("promo4"), "shared-content", md)
	g.Expect(k8sClient.Create(testCtx, newPromotionClass("promo-dedup", 3, 30*time.Second, time.Second))).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("promo4", "promo-dedup"))).To(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo4")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
		g.Expect(a.Status.ContentDigest).To(Equal(contentSHA("shared-content")))
	}).Should(Succeed())

	g.Consistently(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: cmName("promo4", "promo4", 1)}, cm)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}).WithTimeout(time.Second).Should(Succeed())
}

func TestPromotionRefusesLyingContentStamp(t *testing.T) {
	g := NewWithT(t)
	// The content-digest stamp claims bytes the store's own checksum
	// disproves: exactly what a re-stamped overwrite looks like.
	md := stamped("promo5")
	md[artifactsv1.DefaultContentDigestKey] = contentSHA("what-it-claims-to-be")
	fakeStore.PutContent(storeKey("promo5"), "poisoned-content", md)
	g.Expect(k8sClient.Create(testCtx, newPromotionClass("promo-poison", 3, 30*time.Second, time.Second))).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("promo5", "promo-poison"))).To(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo5")
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateKeyConflict))
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonContentMismatch))
	}).Should(Succeed())

	// Refused means refused: no rebuild over the evidence, no adoption.
	g.Consistently(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: cmName("promo5", "promo5", 1)}, cm)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		g.Expect(getArtifact(g, "promo5").Status.ContentDigest).To(BeEmpty())
	}).WithTimeout(time.Second).Should(Succeed())
}

func TestPromotionRegenerateRefusesStrippedStamp(t *testing.T) {
	g := NewWithT(t)
	class := newPromotionClass("promo-regen", 3, 30*time.Second, time.Second)
	class.Spec.Drift = &artifactsv1.DriftSpec{Policy: artifactsv1.DriftPolicyRegenerate}
	md := stamped("promo6r")
	md[artifactsv1.DefaultContentDigestKey] = contentSHA("original")
	fakeStore.PutContent(storeKey("promo6r"), "original", md)
	g.Expect(k8sClient.Create(testCtx, class)).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("promo6r", "promo-regen"))).To(Succeed())

	g.Eventually(func(g Gomega) {
		g.Expect(getArtifact(g, "promo6r").Status.ContentDigest).To(Equal(contentSHA("original")))
	}).Should(Succeed())

	// Digest changed and the content stamp is gone. Regenerate must not
	// rebuild over that evidence; it parks at ContentMismatch.
	fakeStore.PutContent(storeKey("promo6r"), "replaced", stamped("promo6r"))
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo6r")
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateKeyConflict))
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonContentMismatch))
	}).Should(Succeed())

	g.Consistently(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: cmName("promo6r", "promo6r", 1)}, cm)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		g.Expect(getArtifact(g, "promo6r").Status.State).To(Equal(artifactsv1.StateKeyConflict))
	}).WithTimeout(time.Second).Should(Succeed())
}

func TestPromotionDetectsStrippedStampAfterReady(t *testing.T) {
	g := NewWithT(t)
	md := stamped("promo6")
	md[artifactsv1.DefaultContentDigestKey] = contentSHA("original")
	fakeStore.PutContent(storeKey("promo6"), "original", md)
	g.Expect(k8sClient.Create(testCtx, newPromotionClass("promo-strip", 3, 30*time.Second, time.Second))).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("promo6", "promo-strip"))).To(Succeed())

	g.Eventually(func(g Gomega) {
		g.Expect(getArtifact(g, "promo6").Status.ContentDigest).To(Equal(contentSHA("original")))
	}).Should(Succeed())

	// Someone replaces the object with an unpromoted one (spec stamp, no
	// content stamp). With a recorded digest this is not sealable legacy
	// content — it is a replacement outside the promotion path.
	fakeStore.PutContent(storeKey("promo6"), "replaced", stamped("promo6"))
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo6")
		g.Expect(a.Status.State).To(Equal(artifactsv1.StateKeyConflict))
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonContentMismatch))
	}).Should(Succeed())
}

func TestPromotionFailureRetriesWithoutBurningBudget(t *testing.T) {
	g := NewWithT(t)
	g.Expect(k8sClient.Create(testCtx, newPromotionClass("promo-retry", 2, time.Hour, time.Minute))).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("promo7", "promo-retry"))).To(Succeed())

	fakeStore.PutContent(incomingKey("promo7"), "payload-seven", stamped("promo7"))
	fakeStore.FailPromotions(fmt.Errorf("AccessDenied: simulated missing copy permission"))
	defer fakeStore.FailPromotions(nil)
	setCMResult(g, cmName("promo7", "promo7", 1), "ok")

	// The failure is surfaced and retried as a store-level problem: the run
	// is not failed, no new attempt is triggered.
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo7")
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonPromotionFailed))
	}).Should(Succeed())
	g.Consistently(func(g Gomega) {
		a := getArtifact(g, "promo7")
		g.Expect(a.Status.FailedAttempts).To(BeZero())
		g.Expect(a.Status.Attempts).To(Equal(int32(1)))
	}).WithTimeout(2 * time.Second).Should(Succeed())

	// Permission restored: the pending promotion completes on retry. The
	// annotation nudge just triggers an immediate reconcile instead of
	// waiting out the ~30s retry backoff.
	fakeStore.FailPromotions(nil)
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo7")
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		a.Annotations["test-kick"] = "1"
		g.Expect(k8sClient.Update(testCtx, a)).To(Succeed())
	}).Should(Succeed())
	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo7")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.ReadyCondition)).To(BeTrue())
		g.Expect(a.Status.ContentDigest).To(Equal(contentSHA("payload-seven")))
	}).Should(Succeed())
}

func TestPromotionUnsupportedDriverStalls(t *testing.T) {
	g := NewWithT(t)
	class := newPromotionClass("promo-nexus", 3, 30*time.Second, time.Second)
	class.Spec.Store.Driver = "nexus"
	g.Expect(k8sClient.Create(testCtx, class)).To(Succeed())
	g.Expect(k8sClient.Create(testCtx, newArtifact("promo8", "promo-nexus"))).To(Succeed())

	g.Eventually(func(g Gomega) {
		a := getArtifact(g, "promo8")
		g.Expect(apimeta.IsStatusConditionTrue(a.Status.Conditions, fluxmeta.StalledCondition)).To(BeTrue())
		g.Expect(condReason(a, fluxmeta.ReadyCondition)).To(Equal(artifactsv1.ReasonPromotionUnsupported))
	}).Should(Succeed())

	// A misconfigured class burns no pipeline capacity.
	g.Consistently(func(g Gomega) {
		cm := &corev1.ConfigMap{}
		err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: testNS, Name: cmName("promo8", "promo8", 1)}, cm)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}).WithTimeout(time.Second).Should(Succeed())
}
