---
description: Implement an approved issue and open a draft PR
argument-hint: <issue-number-or-description>
---

Implement: $ARGUMENTS

Read AGENTS.md first — the invariants section is the contract; every rule in it
was earned by a real incident.

## Constraints

- Work on a branch, never `main`.
- No scope expansion: implement what the issue asks, nothing adjacent. Anything
  discovered along the way becomes a note in the PR's "Deferred work", not code.
- After touching anything under `api/`, run `make manifests generate` and commit
  the regenerated output — the API server silently prunes fields whose CRDs are
  stale (invariant 4).
- Chart content changed ⇒ bump `version` + `appVersion` in `Chart.yaml`
  (published chart versions are immutable; the release workflow enforces it).
- New behavior needs a test that fails without the change. The envtest suite in
  `internal/controller/` shows the house style.

## Validation order

1. Focused tests for what you changed (`go test ./internal/... -run <TestName>`).
2. `./ci/test.sh` — the whole gate. Never pipe it inside `&&` chains that could
   mask its status; `set -o pipefail` if you must pipe.
3. If envtest binaries are unavailable in this environment: `SKIP_ENVTEST=1
   ./ci/test.sh`, and say so in the PR's Validation section.

## Stop and report instead of continuing when

- A test reveals an architectural misunderstanding, not just a local bug.
- The fix requires expanding scope beyond the issue.
- Existing behavior contradicts what the issue assumes.
- The same failure survives two materially different fix attempts.
- A secret, permission, or external dependency is missing.
- You are considering weakening, deleting, or skipping a test to make a change
  pass — forbidden outright; this is the same rule as a stop condition, not an
  exception to it.

When stopping: state what the evidence proves, what remains uncertain, and the
proposed next action.

## When complete

Re-read the docs adjacent to your change for truthiness, not just accuracy:
does any prose claim become false because of this change? Did you ship
something the README roadmap still lists as future? Does an example still
work if copy-pasted? `ci/docs_test.go` guards the mechanical doc/code pairs —
the prose around them is on you.

Inspect the full diff once more, commit with a conventional message whose body
explains *why* (the git log is the design record), push, and open a **draft**
PR with every section of the PR template filled — Validation with exact
commands and results, Deferred work (`none` if empty), Agent provenance.

Do not merge. Agents open and drive PRs to green; a human merges.
