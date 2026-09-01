# Contributing

Thanks for looking. This project is young (v1alpha1) and small enough that the
best first contribution is often an issue describing what you tried and where
it surprised you.

## Ground rules

**`AGENTS.md` is the operational contract** — invariants, a change map,
testing rules, anti-patterns. It applies to humans exactly as much as to
coding agents; every rule in it was earned by a real incident. Read it before
a non-trivial change.

Working with a coding agent (or as one)?
[docs/agent-workflow.md](docs/agent-workflow.md) is the process contract:
risk tiers, stop conditions, review and triage rules, and the merge gate.

## Developing

Go (see `go.mod` for the version) and `make` are the only requirements; the
test suite needs no cluster, registry, or cloud account.

```sh
./ci/test.sh        # the whole gate: generated files, gofmt, vet, helm lint, unit + envtest
make test           # tests only
make run            # run against your current kubeconfig
```

After touching anything in `api/`, run `make manifests generate` and commit
the generated files — the gate fails otherwise, and the API server silently
prunes fields whose CRD schema is stale (see AGENTS.md invariant 4).

## Pull requests

- One logical change per PR; `./ci/test.sh` green.
- New behavior needs a test that fails without the change. The envtest suite
  in `internal/controller/` shows the house style.
- Chart content changes require a `Chart.yaml` version bump — published chart
  versions are immutable and the release workflow enforces it.
- Commit messages: conventional style (`feat:`, `fix:`, `docs:`, ...) with a
  body that explains *why*. The git log is the design record; write for the
  person doing archaeology in a year.

## Adding a store driver

Think twice: the `http` driver plus a class covers most stores. A dedicated
driver is warranted when the store's semantics can't be expressed in
configuration — see the Nexus driver's comments for the canonical example.
If it is warranted, the change map in `AGENTS.md` lists what a driver touches.
