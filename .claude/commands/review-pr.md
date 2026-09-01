---
description: Independent read-only review of a PR (never review your own implementation)
argument-hint: <pr-number-or-branch>
---

Review PR $ARGUMENTS only. Do not edit, commit, or push.

The reviewer is never the implementer, with one exception: mechanical/docs-only
changes may be self-reviewed, and that self-review must be declared explicitly
in the review. Everything else requires an independent reviewer.

## Independently verify

- Every AGENTS.md invariant the diff could touch — especially: identity-hash
  encoding untouched; generated files regenerated (not hand-edited); chart
  version bumped iff chart content changed; RBAC not widened, no wildcards,
  secrets Role-scoped; `failedWhen`-before-`succeededWhen` ordering preserved.
- The change has a test that fails without it — check the test actually pins
  the behavior, not just executes the code.
- README driver table / lifecycle table / http-auth list consistency
  (`ci/docs_test.go` covers the mechanical pairs; judge the prose around them).
- Conditions and status semantics stay kstatus-conformant (Ready/Reconciling/
  Stalled ownership; no state where nothing requeues and nothing watches).
- For store drivers: Observe never downloads content; Delete of an absent
  object is not an error; foreign-stamp objects are refused, not adopted.

## For every finding, report

- Severity (high / medium / low).
- Exact file and location.
- A concrete failing scenario: inputs/state → wrong outcome. "This looks
  fragile" is not a finding.
- Why the existing tests miss it.
- The smallest valid correction.

Do not pad the review with style commentary, and do not fix anything yourself —
findings go to triage (`/triage-findings`), where a human decides each one.
