# CLAUDE.md

Read **`AGENTS.md`** first — it is the operational contract for this repo:
invariants (several were earned the hard way), a change map, testing rules,
and anti-patterns. Treat it as binding; this file only adds orientation.

## What this is

A Kubernetes controller that reconciles *artifacts* (S3 objects, ECR images,
Nexus/Artifactory assets, AMIs, anything HTTP-observable) as desired state:
declare an artifact by identity, and the controller runs the class's
generator when it's missing, verifies what landed by provenance stamp, and
keeps verifying. See README for concepts; `config/samples/` for real classes.

## Commands

```bash
./ci/test.sh     # the whole gate: generated-files check, gofmt, vet, helm lint, unit + envtest
make test        # tests only (downloads envtest binaries on first run)
make build       # bin/artifact-controller
make manifests generate   # REQUIRED after touching api/ — then commit the generated files
make helm-lint   # chart lint + render
make run         # run against current kubeconfig
```

## Layout

- `api/v1alpha1/` — `Artifact` + `ArtifactClass` types (group `artifacts.kargops.dev`)
- `internal/controller/` — the reconcile state machine
- `internal/store/` — store drivers (s3, oci, ami, artifactory/nexus, httpstore, fake)
- `internal/generator/` — run template rendering + CEL status interpretation
- `internal/hash/` — the frozen canonical identity hash
- `charts/artifact-controller/` — Helm chart (crds/ and manager-clusterrole.yaml are generated)
