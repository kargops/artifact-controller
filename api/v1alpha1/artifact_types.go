package v1alpha1

import (
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ArtifactKind is the kind name of the Artifact type.
	ArtifactKind = "Artifact"

	// ArtifactFinalizer guards deletion so the deletion policy can be applied
	// to the external store object before the CR goes away.
	ArtifactFinalizer = "artifacts.kargops.dev/finalizer"

	// RetryAnnotation resets the failure budget of a Degraded (Stalled)
	// Artifact when set to a value different from status.retryToken.
	RetryAnnotation = "artifacts.kargops.dev/retry"
)

// DeletionPolicy decides what happens to the external store object when the
// Artifact CR is deleted.
type DeletionPolicy string

const (
	// DeletionPolicyOrphan leaves the store object in place (default).
	DeletionPolicyOrphan DeletionPolicy = "Orphan"
	// DeletionPolicyDelete deletes the store object (only if its provenance
	// stamp matches this Artifact's spec hash, or it carries no stamp).
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// ArtifactSpec declares the intent: an artifact, uniquely identified by its
// identity keys, that must exist in the class's external store.
type ArtifactSpec struct {
	// ClassRef names the cluster-scoped ArtifactClass that defines the store
	// driver and the generator able to materialize this artifact.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="classRef is immutable"
	ClassRef meta.LocalObjectReference `json:"classRef"`

	// Identity is the set of keys that uniquely identifies the desired
	// artifact (e.g. source repo, git ref, platform, arch). The canonical
	// hash of this map is the artifact's content address: it determines the
	// store key and is the provenance stamp generators must apply.
	// +required
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="identity is immutable"
	Identity map[string]string `json:"identity"`

	// Params are additional non-identifying inputs passed to the generator
	// template (e.g. runner sizing). They do not affect the spec hash.
	// +optional
	Params map[string]string `json:"params,omitempty"`

	// Interval is how often the store is re-verified once the artifact is
	// Ready, and the general requeue cadence. Jittered by ±10%.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +kubebuilder:default:="1m"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// Suspend pauses reconciliation entirely.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// TTL: when the Artifact object is older than this, the controller stops
	// reconciling and deletes the CR itself (the deletion policy then decides
	// the store object's fate). Zero or unset means reconcile forever.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// DeleteAfter: when the Artifact object is older than this, the artifact
	// is deleted from its store (regardless of deletionPolicy) and the CR
	// transitions to Expired; generators are no longer triggered. Unset means
	// never.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +optional
	DeleteAfter *metav1.Duration `json:"deleteAfter,omitempty"`

	// DeletionPolicy decides what happens to the store object when this CR is
	// deleted. Defaults to Orphan (cache semantics: deleting the intent does
	// not destroy the cache entry).
	// +kubebuilder:validation:Enum=Orphan;Delete
	// +kubebuilder:default:=Orphan
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// GeneratorReference points at the generator run currently owned by this
// Artifact.
type GeneratorReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

// ArtifactStatus is the observed state.
type ArtifactStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SpecHash is the canonical sha256 of spec.identity — the artifact's
	// content address and provenance stamp value.
	// +optional
	SpecHash string `json:"specHash,omitempty"`

	// Key is the rendered store key the artifact is expected at.
	// +optional
	Key string `json:"key,omitempty"`

	// Digest is the store-reported version identifier of the observed object
	// (driver-specific, e.g. "etag:..." or "sha256:...").
	// +optional
	Digest string `json:"digest,omitempty"`

	// State is a display summary; conditions are the source of truth.
	// +optional
	State string `json:"state,omitempty"`

	// Attempts is the number of generator runs created over this Artifact's
	// lifetime (drives deterministic run naming).
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// FailedAttempts is the consecutive failure count driving backoff and the
	// failure budget. Reset on success or via the retry annotation.
	// +optional
	FailedAttempts int32 `json:"failedAttempts,omitempty"`

	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`
	// +optional
	LastFailureMessage string `json:"lastFailureMessage,omitempty"`

	// GeneratorRef points at the in-flight generator run, if any.
	// +optional
	GeneratorRef *GeneratorReference `json:"generatorRef,omitempty"`

	// GeneratorSucceededAt is when the current run was first observed
	// successful; starts the verification grace window.
	// +optional
	GeneratorSucceededAt *metav1.Time `json:"generatorSucceededAt,omitempty"`

	// LastVerifiedTime is when the artifact was last observed present and
	// matching in the store.
	// +optional
	LastVerifiedTime *metav1.Time `json:"lastVerifiedTime,omitempty"`

	// RetryToken records the last honored artifacts.kargops.dev/retry annotation.
	// +optional
	RetryToken string `json:"retryToken,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=art
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Key",type=string,JSONPath=`.status.key`,priority=1
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedAttempts`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Artifact declares an artifact that must exist in an external store, and how
// to produce it if it does not.
type Artifact struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ArtifactSpec `json:"spec,omitempty"`
	// +optional
	Status ArtifactStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the object.
func (in *Artifact) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (in *Artifact) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// GetInterval returns the effective re-verification interval.
func (in *Artifact) GetInterval() time.Duration {
	if in.Spec.Interval.Duration <= 0 {
		return time.Minute
	}
	return in.Spec.Interval.Duration
}

// +kubebuilder:object:root=true

// ArtifactList contains a list of Artifact.
type ArtifactList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Artifact `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Artifact{}, &ArtifactList{})
}
