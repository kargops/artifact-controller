# Agent development workflow

This is the process contract for coding agents working on this repo — who does
what, in what order, and where an agent must stop and wait for a human. It does
not restate architecture or invariants; those live in [AGENTS.md](../AGENTS.md).
The process rules here are ported from a larger sibling project where each was
earned by a real incident, trimmed to this repo's size.

## Risk tiers

Two, decided by which files a change touches:

| Tier | Paths | Review |
|---|---|---|
| **high** | `api/v1alpha1/`, `internal/hash/`, `config/rbac/`, chart RBAC/CRD templates, `.github/workflows/release.yml` | independent reviewer required (never the implementer) |
| **normal** | everything else | independent review preferred; self-review acceptable for mechanical/docs-only changes, declared explicitly |

Docs-only changes are always **normal**, even inside a high path.

## Stop conditions

An implementing agent stops and reports — rather than keeps pushing — when:

- a test reveals an architectural misunderstanding, not just a local bug;
- the fix requires expanding scope beyond the issue;
- existing behavior contradicts what the issue assumes;
- the same failure survives **two materially different fix attempts** (a third
  attempt on the same approach is almost always thrash);
- a secret, permission, or external dependency is missing;
- it is considering weakening, deleting, or skipping a test to make a change
  pass — already forbidden outright; this is the same rule as a stop condition,
  not an exception to it.

When stopping: state what the evidence now proves, what remains uncertain, and
the proposed next action.

## Review and bounded autofix

Automatic autofix is bounded: **one** pass; a second pass only for a direct,
bounded consequence of the first fix. Never an unbounded
review → autofix → re-review loop.

Land a review round as **one** push, not one push per finding — every push to
an open PR starts a fresh CI run against the new head. Fix everything the round
raised, run the gate once, push once. The exception is a fix whose whole
purpose is to see what CI says about it — push that on its own.

## Finding triage

Every review finding gets exactly one outcome, decided by a human:

| Outcome | Meaning |
|---|---|
| **Fix now** | valid and in scope for this PR |
| **Defer** | valid, but moved to a named follow-up issue |
| **Reject** | incorrect or based on a false assumption |
| **Clarify** | more evidence needed before deciding |

Triage is executed via one consolidated instruction naming only the accepted
findings — an agent must not apply a finding (its own or another model's) that
was not explicitly accepted.

## Merge gate

A PR is mergeable when CI is green, no high/medium correctness finding is
unresolved, regression coverage exists for the behavior it changes, deferred
findings have named issues, and the PR template's checklist is complete.

**Agents open and drive PRs to green. They do not merge.**
