package v1alpha1

// Condition types specific to Artifact, complementing the kstatus-compatible
// Ready/Reconciling/Stalled conditions from fluxcd/pkg/apis/meta.
const (
	// ArtifactInStoreCondition reflects the last observation of the artifact
	// in its external store.
	ArtifactInStoreCondition = "ArtifactInStore"

	// ArtifactDriftedCondition reports that the content at the key changed
	// without this controller having generated it.
	ArtifactDriftedCondition = "ArtifactDrifted"

	// GeneratorSucceededCondition reflects the outcome of the current or last
	// generator run.
	GeneratorSucceededCondition = "GeneratorSucceeded"
)

// Condition reasons.
const (
	ReasonArtifactAvailable        = "ArtifactAvailable"
	ReasonArtifactMissing          = "ArtifactMissing"
	ReasonGenerating               = "Generating"
	ReasonAwaitingArtifact         = "AwaitingArtifact"
	ReasonBackoffPending           = "BackoffPending"
	ReasonGeneratorFailed          = "GeneratorFailed"
	ReasonGeneratorSucceeded       = "GeneratorSucceeded"
	ReasonSucceededWithoutArtifact = "SucceededWithoutArtifact"
	ReasonContentDrifted           = "ContentDrifted"
	ReasonStatusUnrecognized       = "GeneratorStatusUnrecognized"
	ReasonProgressDeadlineExceeded = "ProgressDeadlineExceeded"
	ReasonFailureBudgetExhausted   = "FailureBudgetExhausted"
	ReasonKeyConflict              = "KeyConflict"
	ReasonGeneratorNotConfigured   = "GeneratorNotConfigured"
	ReasonStoreUnavailable         = "StoreUnavailable"
	ReasonClassNotFound            = "ClassNotFound"
	ReasonTemplateError            = "TemplateError"
	ReasonExpired                  = "Expired"
	ReasonSuspended                = "Suspended"
	ReasonDeleting                 = "Deleting"
)

// State values surfaced in .status.state. Conditions are the source of truth;
// state is a human/printer-column summary only.
const (
	StatePending          = "Pending"
	StateGenerating       = "Generating"
	StateAwaitingArtifact = "AwaitingArtifact"
	StateReady            = "Ready"
	// StateMissing is observe-only vocabulary: the artifact is absent and this
	// controller is deliberately not the one who will produce it.
	StateMissing     = "Missing"
	StateDegraded    = "Degraded"
	StateExpired     = "Expired"
	StateSuspended   = "Suspended"
	StateKeyConflict = "KeyConflict"
)
