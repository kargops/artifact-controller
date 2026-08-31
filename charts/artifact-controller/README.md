# artifact-controller

A Kubernetes controller that makes **artifacts** — not the pipelines that
build them — the reconciliation target. Declare the artifact you want (an S3
tarball, an ECR image, a Nexus asset, an AMI) by its *identity*; the
controller ensures it exists in its store, runs your pipeline when it does
not, verifies what landed by provenance stamp, and keeps verifying. Delete an
artifact by accident and it comes back.

Full documentation, design notes and a runnable ten-minute quickstart live in
the [project repository](https://github.com/kargops/artifact-controller).

## Install

```sh
helm install artifact-controller oci://ghcr.io/kargops/charts/artifact-controller \
  -n artifact-system --create-namespace
```

The chart installs the CRDs (`Artifact`, `ArtifactClass` under
`artifacts.kargops.dev`), the controller Deployment (2 replicas, leader
election, PodDisruptionBudget), and its RBAC. The controller ships **no
wildcard permissions** — generator-engine access is opt-in per engine.

## Values worth knowing

| Value | Default | Meaning |
|---|---|---|
| `image.repository` / `image.tag` | `ghcr.io/kargops/artifact-controller` / chart appVersion | controller image |
| `replicaCount` | `2` | active/passive via leader election |
| `serviceAccount.name` | `artifact-controller` | **the attachment point for cloud identity** (EKS Pod Identity / IRSA) — read-only against stores |
| `generator.serviceAccount.name` | `artifact-generator` | identity generator runs use — the one that *writes* to stores |
| `generator.engines.batchJob` | `true` | allow `batch/v1` Jobs as generators |
| `generator.engines.tekton` | `false` | allow Tekton PipelineRuns |
| `generator.engines.argoWorkflows` | `false` | allow Argo Workflows |
| `controller.concurrentReconciles` | `4` | parallelism across Artifacts |
| `controller.enableFakeStore` | `false` | in-memory driver, demos only |
| `metrics.enabled` / `metrics.port` | `true` / `8080` | metrics endpoint |
| `env` | `[]` | e.g. `AWS_REGION` when the store's region differs from the cluster's |

Engine toggles render aggregation-labelled ClusterRoles
(`artifacts.kargops.dev/aggregate-to-manager: "true"`), so enabling one later
is a values change with no controller restart.

## After installing

1. Attach cloud identity to the two ServiceAccounts (the controller reads
   stores; generator runs write to them) — the repo README's
   [AWS credentials](https://github.com/kargops/artifact-controller#aws-credentials)
   section has the exact Pod Identity commands and IAM actions per store.
2. Define an `ArtifactClass` (store driver + generator template + CEL status
   interpretation) — start from
   [config/samples](https://github.com/kargops/artifact-controller/tree/main/config/samples).
3. Declare `Artifact`s against it and watch `kubectl get artifacts -w`.

## Notes

- **CRDs are installed and upgraded with the release** (`crds.install: true`,
  the default) and carry `helm.sh/resource-policy: keep`, so `helm uninstall`
  leaves them — and every Artifact object — in place. Set
  `crds.install: false` to manage them out of band (the release's
  `install.yaml`, `config/crd/bases/`, or their own GitOps source); also use
  `false` if the CRDs already exist un-owned by this release, since Helm
  refuses to adopt resources it did not create.
- Published chart versions are immutable; the release workflow enforces it.
