# artifact-controller agent guide

Operational contract for coding agents (and humans in a hurry). The README
explains what the controller *is*; this file is about not breaking it. Every
rule here was earned by a real incident, noted inline.

## Invariants

1. **The canonical identity hash is frozen.** `internal/hash` defines
   `sha256(canonical(spec.identity))`; store keys and provenance stamps in
   real stores depend on its exact encoding. A golden test pins it. Never
   change the encoding — a change orphans every existing artifact.
2. **The API group `artifacts.kargops.dev` is baked into stored objects.**
   Renaming the group or kinds is a cluster migration (delete + recreate CRs),
   not a refactor. Same for `spec.identity` semantics: identity is immutable
   by CEL validation; changing what identity *means* changes every key.
3. **Generated files are tracked and must be regenerated, never edited:**
   `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/*`,
   `config/rbac/role.yaml`, `charts/artifact-controller/crds/*`,
   `charts/artifact-controller/templates/manager-clusterrole.yaml`.
   After touching `api/`, run `make manifests generate` and commit the result;
   `ci/test.sh` fails if you forget.
4. **The schema-skew trap** (hit twice in development): adding a Go field
   without regenerating CRDs means the API server *silently prunes* the field
   on create — in envtest and in clusters. Tests then fail claiming the field
   was never set. If a new field mysteriously has no effect, check the CRD
   before debugging the controller.
5. **Chart discipline:** any change to `charts/` content requires bumping
   `version` and `appVersion` in `Chart.yaml` — published chart versions must
   be immutable (a mutable 0.1.0 once shipped different content under one
   pin). On release tags, `ci/test.sh` asserts Chart.yaml matches the tag.
   Also: duplicate YAML keys are *valid YAML* and pass `helm lint` while
   silently dropping the first value — `ci/chart_test.go` guards this; do not
   weaken it (it once ate the secrets RBAC rule).
6. **Two identities, on purpose.** The controller ServiceAccount only *reads*
   stores; generator runs use `artifact-generator`, which writes. Do not merge
   them or widen the controller's cloud permissions past the README table.
7. **Secrets stay namespaced.** Store drivers resolve credentials only from
   the controller's own namespace. `ArtifactClass` is cluster-scoped, so a
   cluster-wide secrets grant would let anyone who can write a class read any
   secret in the cluster. The grant is a Role — never a ClusterRole.
8. **Run-interpretation semantics:** `failedWhen` is evaluated before
   `succeededWhen`; CEL evaluation errors count as "not matched" (an
   unpopulated status must not fail a run); retries belong to the controller
   (sample Jobs set `backoffLimit: 0` so engines don't hide failures from the
   failure budget).
9. **The `fake` driver is test/demo only**, registered behind
   `--enable-fake-store`. Nothing outside the process can ever satisfy it.

## Change map

| Change | Touch | Then |
|---|---|---|
| API field/type | `api/v1alpha1/` | `make manifests generate`, commit generated files, envtest exercising the field (see invariant 4) |
| Reconcile behavior | `internal/controller/` | an envtest in `artifact_controller_test.go` that fails without the change |
| New store driver | `internal/store/<name>/`, register in `cmd/main.go`, `StoreSpec` + driver enum in `api/`, README driver table | tests against a fake server/endpoint; document stamp + digest semantics honestly (Nexus has no metadata — say so) |
| New auth scheme (http driver) | `internal/store/httpstore/auth*.go`, `HTTPAuthSpec` | a test asserting the header/exchange, and that failures never echo the credential |
| Generator interpretation | `internal/generator/`, class-facing fields in `api/` | CEL evaluated via the real evaluator in tests, not by eye |
| Chart template | `charts/artifact-controller/` | bump `Chart.yaml` version + appVersion; `make helm-lint` |
| Release | tag `vX.Y.Z` | `Chart.yaml` must match the tag (`ci/test.sh` enforces on tags) |

## Testing

`./ci/test.sh` is the whole gate: generated-files freshness, gofmt, vet,
helm lint (when helm is installed), unit tests, and the envtest suite
(binaries auto-downloaded, pinned via `ENVTEST_K8S_VERSION`). Set
`SKIP_ENVTEST=1` only where the envtest download is unreachable.

When verifying in a shell, do not pipe the gate through a filter inside a
`&&` chain — `./ci/test.sh | tail` reports the *filter's* exit status, and a
broken commit was once pushed exactly that way. Use `set -o pipefail` or
check the script's status directly.

## Anti-patterns

Do not:

- edit generated files by hand (invariant 3);
- change the canonical hash encoding (invariant 1);
- infer artifact existence from generator success — verifying the store is
  the controller's entire point (`SucceededWithoutArtifact` exists because
  pipelines lie);
- default drift to `Regenerate` — whatever overwrote the object may write
  again, and two writers fight one key with a build per round;
- add engine-specific Go code where a class template + CEL already works;
- add a store driver the `http` driver covers, unless the store's semantics
  demand it (Nexus earned one: it answers 200-with-empty-list for "absent"
  and deletes by an id only its search API knows);
- publish chart content changes under an existing version;
- weaken RBAC to wildcards to make a generator run work — engine access is
  granted by aggregation-labelled ClusterRoles, per engine, opt-in.

## Definition of done

`./ci/test.sh` passes; generated files committed; chart version bumped if
chart content changed; README updated if commands, drivers, or API surface
changed (the driver table and lifecycle table go stale silently).
