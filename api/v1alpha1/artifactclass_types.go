package v1alpha1

import (
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DefaultStampMetadataKey is the store metadata key generators stamp with the
// artifact's spec hash to establish provenance.
const DefaultStampMetadataKey = "artifact-spec-hash"

// StoreSpec selects and configures the store driver for a class.
type StoreSpec struct {
	// Driver names the registered store driver: "s3" for S3-compatible object
	// stores, "oci" for container registries. "fake" is an in-memory driver
	// for tests and local demos — the controller only registers it when
	// started with --enable-fake-store, and a class referencing an
	// unregistered driver reports StoreUnavailable without running any
	// generator.
	// +kubebuilder:validation:Enum=s3;oci;http;ami;artifactory;nexus;fake
	// +required
	Driver string `json:"driver"`

	// KeyTemplate renders the store key from the artifact. Available fields:
	// .SpecHash, .SpecHex, .Identity, .Params, .Name, .Namespace, .Class.
	// Defaults keep keys purely content-addressed: "{{ .SpecHash }}", except
	// the oci driver which defaults to "{{ .SpecHex }}" because OCI tags may
	// not contain ':'.
	// +optional
	KeyTemplate string `json:"keyTemplate,omitempty"`

	// +optional
	S3 *S3StoreSpec `json:"s3,omitempty"`

	// +optional
	OCI *OCIStoreSpec `json:"oci,omitempty"`

	// +optional
	HTTP *HTTPStoreSpec `json:"http,omitempty"`

	// +optional
	AMI *AMIStoreSpec `json:"ami,omitempty"`

	// RepoManager configures the artifactory and nexus drivers.
	// +optional
	RepoManager *RepoManagerStoreSpec `json:"repoManager,omitempty"`

	// Fake is an in-memory driver for tests and demos.
	// +optional
	Fake *FakeStoreSpec `json:"fake,omitempty"`
}

// S3StoreSpec configures the s3 driver. Credentials come from the default AWS
// chain (IRSA, env, shared config).
type S3StoreSpec struct {
	// +required
	Bucket string `json:"bucket"`
	// +optional
	Region string `json:"region,omitempty"`
	// Endpoint overrides the S3 endpoint (MinIO, LocalStack).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	UsePathStyle bool `json:"usePathStyle,omitempty"`
	// StampMetadataKey is the object metadata key holding the generator's
	// provenance stamp (the artifact spec hash).
	// +kubebuilder:default:="artifact-spec-hash"
	// +optional
	StampMetadataKey string `json:"stampMetadataKey,omitempty"`
}

// OCIStoreSpec configures the oci driver for container registries (ECR,
// GHCR, Harbor, ...). Existence checks are HEAD-manifest only; artifact
// content is never pulled. ECR repositories authenticate via the default AWS
// chain (IRSA); other registries via the docker config keychain.
type OCIStoreSpec struct {
	// Repository is the full repository (registry/name) artifacts of this
	// class live in, e.g.
	// "123456789012.dkr.ecr.us-east-1.amazonaws.com/game-clients".
	// The rendered store key is the TAG within this repository.
	// +required
	Repository string `json:"repository"`

	// Insecure allows plain-HTTP registries (local dev registries).
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// StampAnnotation is the manifest annotation (or image config label)
	// carrying the generator's provenance stamp.
	// +kubebuilder:default:="dev.artifacts.spec-hash"
	// +optional
	StampAnnotation string `json:"stampAnnotation,omitempty"`
}

// DefaultOCIStampAnnotation is the default provenance annotation for the oci
// driver (reverse-domain per OCI annotation conventions).
const DefaultOCIStampAnnotation = "dev.artifacts.spec-hash"

// FakeStoreSpec configures the in-memory fake driver.
type FakeStoreSpec struct{}

// GeneratorSpec defines how to materialize artifacts of this class and how to
// interpret the generator's status, engine-agnostically via CEL.
type GeneratorSpec struct {
	// Template is the full object to create per run (Argo Workflow, Tekton
	// PipelineRun, batch/v1 Job, ...). Every string leaf may use Go template
	// syntax with .Identity, .Params, .SpecHash, .Key, .Name, .Namespace,
	// .Class and .Attempt. metadata.name/namespace and the controller owner
	// reference are set by the controller.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +required
	Template runtime.RawExtension `json:"template"`

	// SucceededWhen is a CEL expression over the generator object (variables:
	// `object`, `status`) that is true when the run succeeded.
	// e.g. Argo:   object.status.phase == 'Succeeded'
	//      Tekton: status.conditions.exists(c, c.type == 'Succeeded' && c.status == 'True')
	// +required
	SucceededWhen string `json:"succeededWhen"`

	// FailedWhen is the CEL expression that is true when the run failed
	// terminally. Evaluation errors count as "not matched".
	// +required
	FailedWhen string `json:"failedWhen"`

	// InProgressWhen closes the status vocabulary. Without it, anything that
	// is neither succeeded nor failed is assumed to be progressing — so a run
	// reporting a state the class never anticipated, or an expression that
	// errors on every evaluation, is indistinguishable from a healthy build.
	// Declare it and a run matching none of the three is reported as
	// Unrecognized instead of being waited on.
	//
	// e.g. Job:    status.active > 0
	//      Argo:   status.phase in ['Pending', 'Running']
	// +optional
	InProgressWhen string `json:"inProgressWhen,omitempty"`

	// ProgressDeadline bounds how long a single run may stay in progress (or
	// unrecognized) before it is counted as a failed attempt. This is what
	// catches a run whose object looks healthy while its execution is wedged
	// — a pod stuck Pending on a missing secret leaves a Job at active:1 with
	// no conditions, which is legitimately "in progress" to any status
	// expression. Unset means only the engine's own timeout applies.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +optional
	ProgressDeadline *metav1.Duration `json:"progressDeadline,omitempty"`
}

// BackoffSpec bounds re-triggering of failing generators.
type BackoffSpec struct {
	// MaxAttempts is the consecutive-failure budget; once exhausted the
	// Artifact is marked Stalled (Degraded) until the retry annotation is
	// set, the spec changes, or the artifact appears in the store.
	// +kubebuilder:default:=5
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +kubebuilder:default:="30s"
	// +optional
	InitialDelay metav1.Duration `json:"initialDelay,omitempty"`

	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +kubebuilder:default:="32m"
	// +optional
	MaxDelay metav1.Duration `json:"maxDelay,omitempty"`
}

// ArtifactClassSpec defines store + generator for a family of artifacts,
// mirroring the StorageClass/PVC split.
type ArtifactClassSpec struct {
	// +required
	Store StoreSpec `json:"store"`

	// +required
	Generator GeneratorSpec `json:"generator"`

	// +optional
	Backoff *BackoffSpec `json:"backoff,omitempty"`

	// Drift configures what happens when the content at the key changes
	// without the controller having generated it. The key addresses the
	// intent, not the bytes, so the same key can legitimately hold different
	// content over time — this is what notices when that was not us.
	// +optional
	Drift *DriftSpec `json:"drift,omitempty"`

	// VerificationGracePeriod is how long after generator success the
	// controller waits for the artifact to appear in the store before
	// counting the run as failed (SucceededWithoutArtifact).
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +kubebuilder:default:="2m"
	// +optional
	VerificationGracePeriod metav1.Duration `json:"verificationGracePeriod,omitempty"`
}

// EffectiveBackoff returns backoff settings with defaults applied.
func (in *ArtifactClass) EffectiveBackoff() (maxAttempts int32, initial, max time.Duration) {
	maxAttempts, initial, max = 5, 30*time.Second, 32*time.Minute
	if b := in.Spec.Backoff; b != nil {
		if b.MaxAttempts > 0 {
			maxAttempts = b.MaxAttempts
		}
		if b.InitialDelay.Duration > 0 {
			initial = b.InitialDelay.Duration
		}
		if b.MaxDelay.Duration > 0 {
			max = b.MaxDelay.Duration
		}
	}
	return
}

// GracePeriod returns the effective verification grace period.
func (in *ArtifactClass) GracePeriod() time.Duration {
	if in.Spec.VerificationGracePeriod.Duration > 0 {
		return in.Spec.VerificationGracePeriod.Duration
	}
	return 2 * time.Minute
}

// KeyTemplate returns the effective key template. Defaulting lives here (not
// in the CRD schema) because it is driver-dependent.
func (in *ArtifactClass) KeyTemplate() string {
	if in.Spec.Store.KeyTemplate != "" {
		return in.Spec.Store.KeyTemplate
	}
	if in.Spec.Store.Driver == "oci" {
		return "{{ .SpecHex }}"
	}
	return "{{ .SpecHash }}"
}

// StampMetadataKey returns the effective provenance-stamp metadata key
// (lowercased; drivers normalize metadata keys to lowercase).
func (in *ArtifactClass) StampMetadataKey() string {
	switch {
	case in.Spec.Store.S3 != nil && in.Spec.Store.S3.StampMetadataKey != "":
		return strings.ToLower(in.Spec.Store.S3.StampMetadataKey)
	case in.Spec.Store.RepoManager != nil && in.Spec.Store.RepoManager.StampPropertyKey != "":
		return strings.ToLower(in.Spec.Store.RepoManager.StampPropertyKey)
	case in.Spec.Store.AMI != nil && in.Spec.Store.AMI.StampTagKey != "":
		return strings.ToLower(in.Spec.Store.AMI.StampTagKey)
	case in.Spec.Store.OCI != nil && in.Spec.Store.OCI.StampAnnotation != "":
		return strings.ToLower(in.Spec.Store.OCI.StampAnnotation)
	case in.Spec.Store.Driver == "oci":
		return DefaultOCIStampAnnotation
	}
	return DefaultStampMetadataKey
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=artclass
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.spec.store.driver`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ArtifactClass defines how a family of artifacts is stored and produced.
type ArtifactClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ArtifactClassSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ArtifactClassList contains a list of ArtifactClass.
type ArtifactClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArtifactClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArtifactClass{}, &ArtifactClassList{})
}

// ProgressDeadline returns how long a run may stay in progress before it is
// counted as failed. Zero means no deadline beyond the engine's own.
func (in *ArtifactClass) ProgressDeadline() time.Duration {
	if d := in.Spec.Generator.ProgressDeadline; d != nil {
		return d.Duration
	}
	return 0
}

// DriftPolicy decides what happens when the store object's content changes
// without this controller having generated it.
type DriftPolicy string

const (
	// DriftPolicyWarn reports the change and stays Ready (default). Safe:
	// whatever overwrote the object may write again, and regenerating would
	// put two systems in a build-per-round fight over one key.
	DriftPolicyWarn DriftPolicy = "Warn"
	// DriftPolicyIgnore re-baselines silently.
	DriftPolicyIgnore DriftPolicy = "Ignore"
	// DriftPolicyRegenerate treats drifted content as missing and rebuilds.
	DriftPolicyRegenerate DriftPolicy = "Regenerate"
)

// DriftPolicy returns the effective policy for content that changed underneath
// the controller.
func (in *ArtifactClass) DriftPolicy() DriftPolicy {
	if in.Spec.Drift != nil && in.Spec.Drift.Policy != "" {
		return in.Spec.Drift.Policy
	}
	return DriftPolicyWarn
}

// DriftSpec configures content-change detection.
type DriftSpec struct {
	// Policy on detecting content this controller did not generate.
	// +kubebuilder:validation:Enum=Warn;Ignore;Regenerate
	// +kubebuilder:default:=Warn
	// +optional
	Policy DriftPolicy `json:"policy,omitempty"`
}

// HTTPAuthType names a supported authentication scheme for the http driver.
type HTTPAuthType string

const (
	HTTPAuthNone   HTTPAuthType = "none"
	HTTPAuthBearer HTTPAuthType = "bearer"
	HTTPAuthBasic  HTTPAuthType = "basic"
	// HTTPAuthHeader sets a verbatim header value from a secret, for stores
	// with a proprietary scheme (Artifactory's X-JFrog-Art-Api, for example).
	HTTPAuthHeader HTTPAuthType = "header"
	// HTTPAuthSigV4 signs the request with the controller's ambient AWS
	// identity. No secret is involved — that is the point of Pod Identity and
	// IRSA — and one implementation reaches every AWS service with a REST API.
	HTTPAuthSigV4 HTTPAuthType = "sigv4"
	// HTTPAuthClientCredentials exchanges a client id and secret for a
	// short-lived token (Microsoft Graph, Keycloak, most machine-to-machine
	// APIs). The token is cached until shortly before it expires.
	HTTPAuthClientCredentials HTTPAuthType = "clientCredentials"
)

// HTTPRequestSpec is one templated request. Only .Key is available: everything
// identity-derived is already folded into the key by keyTemplate.
type HTTPRequestSpec struct {
	// +kubebuilder:validation:Enum=GET;HEAD;POST;DELETE
	// +kubebuilder:default:=HEAD
	// +optional
	Method string `json:"method,omitempty"`

	// +required
	URL string `json:"url"`

	// +optional
	Headers map[string]string `json:"headers,omitempty"`
}

// HTTPAuthSpec selects an auth scheme and where its credentials live. The set
// is closed on purpose: schemes needing bespoke signing belong in a driver.
type HTTPAuthSpec struct {
	// +kubebuilder:validation:Enum=none;bearer;basic;header;sigv4;clientCredentials
	// +kubebuilder:default:=none
	// +optional
	Type HTTPAuthType `json:"type,omitempty"`

	// SecretRef names a secret in the controller's namespace.
	// +optional
	SecretRef *meta.LocalObjectReference `json:"secretRef,omitempty"`

	// +optional
	TokenKey string `json:"tokenKey,omitempty"`
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
	// HeaderName is the header set by the "header" scheme.
	// +optional
	HeaderName string `json:"headerName,omitempty"`

	// Region and Service are required by "sigv4" and name what the signature
	// is scoped to, e.g. region eu-central-1, service execute-api or s3.
	// +optional
	Region string `json:"region,omitempty"`
	// +optional
	Service string `json:"service,omitempty"`

	// TokenURL is the token endpoint for "clientCredentials".
	// +optional
	TokenURL string `json:"tokenURL,omitempty"`
	// +optional
	ClientIDKey string `json:"clientIDKey,omitempty"`
	// +optional
	ClientSecretKey string `json:"clientSecretKey,omitempty"`
	// Scopes requested with the token, e.g. https://graph.microsoft.com/.default
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

// HTTPStoreSpec configures the http driver: how to ask a store whether an
// artifact exists, and how to read the answer. Expressions are CEL over the
// response, with variables code (int), headers (lowercased map), body
// (truncated to 64KiB) and json (decoded body, empty when not JSON).
type HTTPStoreSpec struct {
	// +required
	Observe HTTPRequestSpec `json:"observe"`

	// Delete is required only for classes using deleteAfter or
	// deletionPolicy: Delete.
	// +optional
	Delete *HTTPRequestSpec `json:"delete,omitempty"`

	// Exists decides presence. Defaults to a 2xx response.
	// e.g. "code == 200", or "code == 200 && json.assets.size() > 0"
	// +optional
	Exists string `json:"exists,omitempty"`

	// Digest extracts the store's own content identifier, which is what makes
	// drift detectable. Optional: a store without one simply cannot report
	// content changes.
	// e.g. "headers['etag']"
	// +optional
	Digest string `json:"digest,omitempty"`

	// Stamp extracts the provenance value the generator wrote.
	// e.g. "headers['x-artifact-spec-hash']" or "json.metadata.specHash"
	// +optional
	Stamp string `json:"stamp,omitempty"`

	// StampKey names the metadata key the extracted stamp is published under;
	// it must match what the class verifies against.
	// +optional
	StampKey string `json:"stampKey,omitempty"`

	// +optional
	Auth *HTTPAuthSpec `json:"auth,omitempty"`

	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// AMIStoreSpec configures the ami driver. Credentials come from the default
// AWS chain (Pod Identity, IRSA, env), so no secret is referenced here.
type AMIStoreSpec struct {
	// +optional
	Region string `json:"region,omitempty"`

	// Owner scopes the search. Defaults to "self"; set an account id to
	// observe images shared from elsewhere.
	// +optional
	Owner string `json:"owner,omitempty"`

	// StampTagKey is the EC2 tag carrying the generator's provenance stamp.
	// +kubebuilder:default:="artifact-spec-hash"
	// +optional
	StampTagKey string `json:"stampTagKey,omitempty"`

	// DeleteSnapshots also removes the EBS snapshots backing the image when an
	// artifact is deleted. Off by default: deregistering an image is
	// recoverable while its snapshots survive, and deleting them is not.
	// Leaving it off means deleted artifacts leave snapshots behind, which
	// cost money — pair it with a lifecycle process either way.
	// +optional
	DeleteSnapshots bool `json:"deleteSnapshots,omitempty"`
}

// RepoManagerStoreSpec configures the artifactory and nexus drivers. The
// rendered key is the artifact's path within the repository.
type RepoManagerStoreSpec struct {
	// BaseURL is the service root, e.g. https://artifactory.internal/artifactory
	// or https://nexus.internal.
	// +required
	BaseURL string `json:"baseURL"`

	// +required
	Repository string `json:"repository"`

	// SecretRef names a secret in the controller's namespace. The scheme is
	// chosen from the keys present: apiKey (Artifactory), token (bearer), or
	// username plus password.
	// +optional
	SecretRef *meta.LocalObjectReference `json:"secretRef,omitempty"`

	// StampPropertyKey is the Artifactory property carrying the provenance
	// stamp. Nexus has no arbitrary asset metadata, so classes there rely on
	// the key being content-addressed instead.
	// +kubebuilder:default:="artifact-spec-hash"
	// +optional
	StampPropertyKey string `json:"stampPropertyKey,omitempty"`

	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}
