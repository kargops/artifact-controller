---
name: audit-issues
description: Audit current artifact-controller issues against repository reality and external engineering expectations, draft material concern comments, and rank the implementation-ready subset by simplicity without changing repository or GitHub state.
argument-hint: "[issue numbers or scope]"
context: fork
agent: issue-auditor
background: false
---

Audit and rank artifact-controller issues within this optional scope: $ARGUMENTS

If no scope was supplied, discover current candidates mechanically. Follow the `issue-auditor`
instructions exactly. Return the complete audit report, concern-comment drafts, and selection. Do
not post comments, edit issues, change repository state, or implement the selected issue.

## Decision gap

`SELECTED: none` is a dead end for the caller unless the report says what would end it. This repo
has no planning gate (`docs/agent-workflow.md`), so the usual reason the queue is empty is a
candidate that is technically sound but waits on one bounded human input: the `agent/ready` label
on an issue the audit found ready to label, a maintainer decision the issue leaves open (for
example, which `driftPolicy`/`managementPolicy` behavior a new case should get — the issue then
carries or should carry `human/decision-required`), or reporter information a bug report omitted
(controller version, the class, or the Artifact's `status` from the bug-report form). That is the
cheapest exclusion to clear.

So whenever the selection is `SELECTED: none` **and at least one candidate was excluded solely for
a missing `agent/ready` label, one open decision, or one missing report detail**, end the report
with a `DECISION GAP` section naming exactly one issue — the one whose answer would return the
most value soonest. Judge that on the same simplicity ranking used for selection, then break ties
on the README roadmap order ("Roadmap, roughly in order of value") and on how much of the change
surface the issue body already pins down.

Report it in this shape, so a caller can act on it without re-deriving anything:

```
DECISION GAP: #N — <title>
Blocked only by: <missing agent/ready | the single open decision or missing report detail, stated as a question>
Asked in: <the drafted or existing concern comment that poses it, or "n/a — label only">
Labels: <type/*, area/*, agent/*, human/*>
Risk tier: <high (<the high-tier path it touches>) | normal>
Change surface: <the files/API fields/drivers the issue already names>
Regression handle: <the observable behavior a focused unit test or envtest could prove>
Other candidates waiting on a single decision: #A, #B, ...
```

State `DECISION GAP: none` when every excluded candidate failed for some other reason — a
technical defect in the issue, an invariant it would break, an active PR, an unsatisfied
dependency or `agent/blocked`, or scope beyond one bounded change. A missing `agent/ready` label, a
single open decision, or one missing report detail is the *only* exclusion this section covers; do not use it to relitigate an issue's technical defects, and do not use it
for an issue that would change an `AGENTS.md` invariant (the identity hash encoding, the API
group, identity semantics) — that is a migration decision, not a single question.

This section is still only a report. Naming the gap does not authorize posting, answering,
labelling, or implementing it — the read-only rule above is unchanged, and running
`/implement-issue` is the caller's decision.
