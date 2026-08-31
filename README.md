# artifact-controller

A Kubernetes controller that makes **artifacts** — not the pipelines that
build them — the reconciliation target.

You declare the artifact you want (an S3 tarball, an ECR image, a test bundle)
by its *identity*: the set of keys that uniquely determines it (source repo,
git ref, platform, arch, ...). The controller continuously ensures that
artifact exists in its external store. If it does not, it instantiates the
class's **generator** (an Argo Workflow, Tekton PipelineRun, batch Job — any
Kubernetes object), waits for it to succeed, verifies the artifact landed, and
keeps re-verifying forever. Delete the artifact from the store by accident and
the pipeline is re-triggered. CI inverted into desired state.

```
Artifact (intent)            ArtifactClass (how)              External store
  identity ──hash──► key ──► generator template + CEL ──run──► s3://bucket/<hash>
      │                                                            ▲
      └────────────── observe / verify / regenerate ───────────────┘
```

> **Status: v1alpha1.** The design is stable and the loop is proven end to end
> on a real cluster (build → verify → self-heal), but the API may still change
> and several store drivers have only been exercised against test doubles.
> Expect sharp edges; file issues freely.

**New here? Start with the [quickstart](docs/quickstart.md)** — one kind
cluster, no cloud account, ten minutes to watching an artifact self-heal.

## The identity contract (content addressing)

`status.specHash = sha256(canonical(spec.identity))` is the artifact's
content address:

- **The store key is a function of the hash** (`keyTemplate`), so
  existence-checking, verification, and dedup collapse into a single
  deterministic HEAD.
- **Generators must stamp** the object they upload with the hash (S3 object
  metadata `artifact-spec-hash`; OCI manifest annotation or image label
  `dev.kargops.artifacts.spec-hash`). A present-but-differently-stamped object is a
  `KeyConflict`: the controller will neither adopt nor overwrite nor delete it.
- `spec.identity` and `spec.classRef` are **immutable** (CEL-validated):
  changing intent means creating a new Artifact.

The canonical hash encoding is frozen by a golden test in `internal/hash`.

## API

```yaml
apiVersion: artifacts.kargops.dev/v1alpha1
kind: Artifact
metadata:
  name: game-client-1-4-2-win-x86
spec:
  classRef: { name: s3-game-clients }
  identity:
    source: client-repo.git
    gitRef: v1.4.2
    platform: windows
    arch: x86
  interval: 5m         # store re-verification cadence (jittered ±10%)
  ttl: 0s              # >0: delete this CR after that age (stop reconciling)
  deleteAfter: 720h    # >0: delete the artifact from the store after that age
  deletionPolicy: Delete   # Orphan (default) | Delete — store object's fate on CR delete
```

The cluster-scoped `ArtifactClass` is the StorageClass analog: store driver +
generator template + engine-agnostic status interpretation:

```yaml
apiVersion: artifacts.kargops.dev/v1alpha1
kind: ArtifactClass
metadata:
  name: s3-game-clients
spec:
  store:
    driver: s3
    keyTemplate: "clients/{{ .SpecHash }}.tar.gz"
    s3: { bucket: game-clients, region: eu-west-1 }
  generator:
    template:            # any k8s object; string leaves are Go templates
      apiVersion: tekton.dev/v1
      kind: PipelineRun
      spec:
        pipelineRef: { name: build-game-client }
        params:
          - { name: git-ref, value: "{{ .Identity.gitRef }}" }
          - { name: s3-key,  value: "{{ .Key }}" }
          - { name: stamp,   value: "{{ .SpecHash }}" }
    succeededWhen:  "status.conditions.exists(c, c.type == 'Succeeded' && c.status == 'True')"
    failedWhen:     "status.conditions.exists(c, c.type == 'Succeeded' && c.status == 'False')"
    inProgressWhen: "status.conditions.exists(c, c.type == 'Succeeded' && c.status == 'Unknown')"
    progressDeadline: 20m
  backoff: { maxAttempts: 5, initialDelay: 1m, maxDelay: 32m }
  verificationGracePeriod: 2m
  drift: { policy: Warn }
```

Adding an engine is configuration, not code — e.g. Argo Workflows is
`succeededWhen: object.status.phase == 'Succeeded'`, a Job is
`status.conditions.exists(c, c.type == 'Complete' && c.status == 'True')`.

### Interpreting a run

`succeededWhen` and `failedWhen` are required; the other two close gaps that
only show up in production.

**`inProgressWhen` closes the vocabulary.** Without it, anything that is
neither succeeded nor failed is *assumed* to be progressing — so a run
reporting a state the class never described, and an expression that errors on
every evaluation, both look exactly like a healthy build. Declare it and a run
matching none of the three reports `GeneratorStatusUnrecognized`, naming the
status it actually saw. It stays in `Generating`, because not understanding a
run is a diagnosis, not a verdict.

**`progressDeadline` bounds a run that never reaches a terminal state.** No
status expression can see a wedged execution: a pod stuck `Pending` on a
missing secret leaves its Job at `active: 1` with no conditions, which is
legitimately "in progress" to any vocabulary. Only elapsed time catches it. On
expiry the run is deleted — so the next attempt does not race a wedged one
still holding its deterministic name — and the attempt counts as failed.

Template fields: `.Identity`, `.Params`, `.SpecHash` (`sha256:<hex>`),
`.SpecHex` (bare hex, for OCI tags), `.Key`, `.Name`, `.Namespace`, `.Class`,
`.Attempt`.

## Store drivers

| Driver | Backend | Existence check | Stamp location | Key default |
|---|---|---|---|---|
| `s3` | S3 / MinIO / LocalStack | `HeadObject` (no download) | object metadata `artifact-spec-hash` | `{{ .SpecHash }}` |
| `oci` | ECR / GHCR / Harbor / any registry | manifest `GET` (no layer pulls) | manifest annotation `dev.kargops.artifacts.spec-hash`, falling back to image config label | `{{ .SpecHex }}` |
| `artifactory` | JFrog Artifactory | storage API, one call | artifact property `artifact-spec-hash` | `{{ .SpecHash }}` |
| `nexus` | Sonatype Nexus | asset search | none — Nexus has no asset metadata, so the key carries provenance | `{{ .SpecHash }}` |
| `ami` | EC2 machine images | `DescribeImages` by name | EC2 tag `artifact-spec-hash` | `{{ .SpecHash }}` |
| `http` | anything answering HTTP | your request, your CEL | wherever you say it is | `{{ .SpecHash }}` |
| `fake` | in-memory, `--enable-fake-store` only | — | — | — |

Credentials are ambient for the first two: the s3 driver uses the default AWS
chain (IRSA, Pod Identity, env), and the oci driver uses the ECR credential
helper for ECR registries and the docker config keychain elsewhere.

OCI tags may not contain `:`, which is why oci classes address by `.SpecHex`.
Deletion removes the manifest by digest (on ECR this retires every tag on it),
then best-effort removes the tag; registries that forbid tag deletion are
tolerated.

### The `http` driver

Exists so that a new store costs a class, not a controller release. The store's
API is described in the class: a templated request, CEL over the response, and
one of a small closed set of auth schemes.

```yaml
store:
  driver: http
  keyTemplate: "{{ .SpecHex }}.tar.gz"
  http:
    observe: { method: GET, url: "https://nexus.internal/…/search/assets?name={{ .Key }}" }
    exists: "code == 200 && json.items.size() > 0"   # default: any 2xx
    digest: "json.items[0].checksum.sha256"          # optional
    stamp:  "json.items[0].attributes.specHash"      # optional
    delete: { method: DELETE, url: "https://nexus.internal/…/components/{{ .Key }}" }
    auth:   { type: basic, secretRef: { name: nexus-credentials } }
```

Expressions see `code` (int), `headers` (lowercased map), `body`, and `json`
(the decoded body, an empty map when the response is not JSON). Auth is
`none`, `bearer`, `basic`, `header` (a verbatim header value from a secret, for
proprietary schemes like Artifactory's `X-JFrog-Art-Api`), `sigv4` (requests
signed with the controller's ambient AWS identity — no secret involved), or
`clientCredentials` (OAuth2 exchange with the token cached until expiry — the
Microsoft Graph / Keycloak flow). Anything needing bespoke, non-standard
signing belongs in a real driver, not in configuration.

Two deliberate limits. Response bodies are **capped at 64KiB**, so an existence
probe can never quietly become a download. And credentials are read **only from
the controller's own namespace**: `ArtifactClass` is cluster-scoped, so a
cluster-wide secret grant would let anyone able to write a class read any secret
in the cluster — the driver's permission is a namespaced `Role`, never the
`ClusterRole`.

## Lifecycle semantics

| Mechanism | Question it answers | Effect |
|---|---|---|
| `interval` | how fresh is "exists"? | jittered re-verification requeue |
| generator runs | artifact missing? | owned, deterministically named attempt objects (`<name>-<hash8>-rN`) |
| `backoff` + `maxAttempts` | generator keeps failing? | capped exponential backoff, then **Degraded** (`Stalled=True`), no more runs |
| `artifacts.kargops.dev/retry: <token>` annotation | human says try again | resets the failure budget once per token |
| `verificationGracePeriod` | generator "succeeded" but store empty? | wait, then count as failed attempt (`SucceededWithoutArtifact`) |
| `deleteAfter` | artifact GC | one-shot store deletion at age, CR parks as **Expired** |
| `ttl` | intent GC | controller deletes the CR at age; finalizer applies `deletionPolicy` |
| `deletionPolicy` | CR deleted → store object? | `Orphan` (default) or `Delete` (only if stamp matches) |
| `drift.policy` | content changed and we didn't do it? | `Warn` (default), `Ignore`, or `Regenerate` |
| `suspend` | pause everything | no observation, no runs |

Status follows kstatus conventions (`Ready` / `Reconciling` / `Stalled` from
`fluxcd/pkg/apis/meta`) plus `ArtifactInStore`, `GeneratorSucceeded` and
`ArtifactDrifted` conditions, and a `status.state` printer column
(`Pending → Generating → AwaitingArtifact → Ready | Degraded | Expired | KeyConflict | Suspended`).

### Drift

The key addresses the *intent*, not the bytes, so the same key can legitimately
hold different content over time — and a provenance stamp cannot see an
overwrite that preserves it. What can is the store's own digest, which the
writer does not compute and so cannot forge.

The controller baselines `status.digest` at each verification and asks one
question when it changes: **did we cause this?** A change with a generator run
of ours in between re-baselines silently. A change with no run in between
raises `ArtifactDrifted` and an event.

`Warn` is the default rather than `Regenerate`, because whatever overwrote the
object may write again — regenerating would put two systems in a build-per-round
fight over one key. `Regenerate` is available when you know you are the only
writer.

Drift needs nothing from the generator, which is what makes it universal: it
works for engines with no status surface at all (`PyTorchJob`, `RayJob`,
`SparkApplication`, a bare Pod), because the controller only ever asks the
store.

Signal quality varies by store, and it is worth knowing which you have:
an OCI manifest digest is a true content address; an S3 `ETag` is not a content
hash for multipart uploads but does change when the object does; a generic HTTP
`ETag` may be weak or regenerated per response, so treat `http` drift as opt-in.

## The generator contract

A pipeline referenced by a class must:

1. **Write to the key it is given** (`{{ .Key }}`).
2. **Stamp the object** with `{{ .SpecHash }}` (S3 object metadata / OCI
   annotation or label).
3. Report success/failure in its own status (interpreted by the class's CEL).

Everything else — naming, ownership, retries, dedup — is the controller's job.
Two Artifacts with the same identity share the same key, so a second CR of the
same intent becomes Ready by observation without triggering a duplicate build
(cross-namespace singleflight of *in-flight* runs is on the roadmap).

## Install

Helm, from GHCR (chart and image are published by the release workflow):

```sh
helm install artifact-controller oci://ghcr.io/kargops/charts/artifact-controller \
  --version 0.8.0 -n artifact-system --create-namespace
```

Or the flat bundle (CRDs + RBAC + Deployment, pinned to the release's image):

```sh
kubectl apply --server-side -f \
  https://github.com/kargops/artifact-controller/releases/latest/download/install.yaml
```

Kustomize users can consume `config/default` as a remote base instead.

Then grant the controller the generator engines your classes use. The chart
does this via values (`generator.engines.batchJob` is on by default; Tekton
and Argo Workflows are toggles). With the flat bundle, apply the matching
ClusterRole from `config/rbac/generator-engines/` — they carry the
`artifacts.kargops.dev/aggregate-to-manager: "true"` label and aggregate into
the manager's role at runtime, no redeploy needed.

The controller holds **no wildcard cluster permissions**: out of the box it can
only touch its own CRs, events, leases, and secrets *in its own namespace*
(which the `http` driver needs for store credentials). Engine access is opt-in
and explicit.

Working classes to start from live in [`config/samples/`](config/samples/):
S3 + Tekton, S3 + Argo Workflows, ECR images built by a Tekton kaniko
pipeline, an HTTP-driver class for Nexus, and an Entra ID account observed via
Microsoft Graph (read its header warning before pointing it at people).

### Availability

The chart defaults to two replicas with leader election, a PodDisruptionBudget
of `minAvailable: 1`, and a soft hostname topology spread. Leader election means
this is **active/passive** — the standby reconciles nothing until it takes the
lease — so it buys failover, not throughput; raise `concurrentReconciles` for
that. A terminating leader releases its lease rather than making its successor
wait out the ~15s expiry, so a rollout costs seconds rather than a stalled
interval.

Leader election matters even at one replica: it is what stops the old and new
pods from both reconciling during a rolling update.

### AWS credentials

The chart renders **two** ServiceAccounts, because the halves of the system
want opposite permissions: `artifact-controller` only ever *reads* the store,
while the generator runs it starts *write* to it. Splitting them means a bug in
the reconcile loop cannot delete or overwrite an artifact, and a build cannot
read the controller's identity.

| ServiceAccount | Needs |
|---|---|
| `artifact-controller` | read-only on the stores your classes observe |
| `artifact-generator` | write access for the runs, referenced from class templates by `serviceAccountName` |

Both names and the namespace are the attachment point for cloud identity, so
keep them stable — an association binds to the exact pair, and a rename grants
nothing, silently.

On EKS, prefer **Pod Identity** (needs the `eks-pod-identity-agent` addon; no
annotation on the ServiceAccount):

```sh
aws eks create-pod-identity-association \
  --cluster-name <cluster> \
  --namespace artifact-system \
  --service-account artifact-controller \
  --role-arn arn:aws:iam::<account>:role/artifact-controller
```

IRSA works too — uncomment the `eks.amazonaws.com/role-arn` annotation in
`config/rbac/service_account.yaml` and trust the cluster OIDC provider for
`system:serviceaccount:artifact-system:artifact-controller`. Either way
credentials arrive through the default AWS chain, which both the s3 driver and
the ECR keychain already use, so no controller configuration changes.

The role needs, scoped to the buckets and repositories your classes reference:

| Store | Actions |
|---|---|
| s3 | `s3:GetObject` (HeadObject is authorized as GetObject), `s3:DeleteObject` when any class uses `deleteAfter` or `deletionPolicy: Delete` |
| oci (ECR) | `ecr:GetAuthorizationToken` (resource `*`), `ecr:BatchGetImage`, `ecr:DescribeImages`, plus `ecr:BatchDeleteImage` for deletion |

The generator's role is whatever its runs need to *write* — for ECR that is the
push set (`BatchCheckLayerAvailability`, `InitiateLayerUpload`,
`UploadLayerPart`, `CompleteLayerUpload`, `PutImage`), which an existing build
role usually already grants.

### CI

GitHub Actions (`.github/workflows/ci.yml`) runs `ci/test.sh` on pushes
to main and on every PR — and that script is the whole gate — generated files
current, gofmt, vet, unit tests and the envtest suite — so any CI system runs
one command:

```sh
./ci/test.sh
```

Set `SKIP_ENVTEST=1` where a runner cannot reach the envtest binary download;
the integration suite then skips itself and the unit tests still gate the run.

### Cutting a release

Releases are tag-driven (`.github/workflows/release.yml`):

1. Bump `version` and `appVersion` in `charts/artifact-controller/Chart.yaml`
   (the gate refuses a tag they do not match).
2. `git tag vX.Y.Z && git push origin vX.Y.Z`

The workflow runs the gate, pushes the multi-arch image to
`ghcr.io/kargops/artifact-controller`, pushes the chart to
`oci://ghcr.io/kargops/charts`, renders `dist/install.yaml` pinned to that
image, and creates a GitHub release carrying it. It **refuses to republish an
existing chart version** — published versions are immutable.

### Artifact Hub

The chart carries `artifacthub.io/*` annotations (CRD cards, examples, images,
changelog) and a chart-level README, so the listing is rich out of the box. To
(re)list it: add a repository in the Artifact Hub control panel with kind
**Helm charts** and url `oci://ghcr.io/kargops/charts/artifact-controller`
(one OCI repo = one chart; versions are discovered from semver tags). Then
link it for the verified-publisher badge:

```sh
make artifacthub-metadata AH_REPO_ID=<uuid from the control panel>
```

First-release note: GHCR packages start private. Make the two packages
(`artifact-controller`, `charts/artifact-controller`) public in the org's
package settings or anonymous installs will fail.

## Development

No cluster, registry, or AWS account needed for the test suite (envtest +
in-memory store + an in-process OCI registry):

```sh
make test      # unit + envtest integration suite
make build     # bin/artifact-controller
make install   # apply just the CRDs to the current kubecontext
make run       # run the controller locally against the current kubecontext
```

## Design provenance

Deliberately assembled from proven parts rather than invented (or forked)
wholesale — see NOTICE:

- **flux source-controller / fluxcd/pkg**: runtime conditions & patch
  machinery, kstatus condition taxonomy, `interval`/`suspend` conventions,
  jittered polling. Not forked: source-controller is read-only (it mirrors
  what exists; it never causes anything to exist) and fetches content, while
  this controller orchestrates generation and only reads store metadata.
- **cert-manager**: intent object → owned attempt objects, failure budget,
  retry-by-annotation.
- **Crossplane**: Observe/Delete driver contract, Orphan/Delete deletion
  policies.

Roadmap, roughly in order of value:

- **Dedup across Artifacts.** Two Artifacts with the same identity resolve to
  the same key, so the second becomes Ready by observation — but if both are
  missing at once they each start a run, because run names derive from the
  Artifact's name. Cluster-level singleflight would collapse that.
- **`contentDigestFrom`** — read a digest the generator reports (Tekton
  results, Argo outputs, Flux `status.artifact.digest`) as *provenance*. Note
  it cannot replace the store digest for drift: a run object is ephemeral, so
  it can only ever be captured once, never re-observed.
- **Observe-only Artifacts**, in the spirit of Crossplane's
  `managementPolicies: [Observe]`: track and report on something another system
  produces, never generate it.
- Event-driven requeues (S3 EventBridge / ECR events), Prometheus metrics, and
  optionally emitting Flux `ExternalArtifact` objects (RFC-0012) so
  kustomize/helm controllers can consume generated manifests.

## License

Apache-2.0.
