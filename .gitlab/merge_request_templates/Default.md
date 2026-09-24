<!-- One logical change per merge request. AGENTS.md is the operational
contract — the checklist below is its invariants in checkbox form. GitLab
applies this template to every new MR because it is named Default.md; it
mirrors .github/PULL_REQUEST_TEMPLATE.md, and ci/templates_test.go keeps the
two in step. -->

## Summary

<!-- What this changes and why. The git log is the design record; write for the
person doing archaeology in a year. -->

## Changes

-

## Validation

<!-- Exact commands run and their results, e.g. `./ci/test.sh` (pass),
`SKIP_ENVTEST=1 ./ci/test.sh` (pass — no envtest binaries in this environment). -->

-

## Invariant checklist

<!-- Tick every box. A box that needed no action still gets ticked — "checked,
nothing to update" is the point. Details: AGENTS.md. -->

- [ ] Generated files regenerated and committed (`make manifests generate`) — or no `api/` change
- [ ] `Chart.yaml` `version` + `appVersion` bumped — or no chart content change
- [ ] A regression test exists that fails without this change and passes with it — or this is docs/mechanical only
- [ ] README driver table / lifecycle table / http-auth list checked against the change (tick when no update was needed too — `ci/docs_test.go` enforces the pairs it can)
- [ ] Docs adjacent to the change re-read for truthiness: no prose claim made false, no shipped feature still listed on the roadmap, examples still copy-paste
- [ ] No RBAC widened; no wildcard verbs/resources; secrets remain Role-scoped
- [ ] The canonical identity hash (`internal/hash`) encoding untouched

## Deferred work

<!-- Anything this MR deliberately does not do, each with a named follow-up
issue. Write `none` rather than leaving this blank. -->

none

## Agent provenance

<!-- Fill in if this MR was planned, implemented, or reviewed by an agent.
Write `n/a` per role if not applicable. -->

- Planner:
- Implementer:
- Reviewers:

<!-- If this MR resolves an issue, write `Closes #N` so merging auto-closes it.
Quick actions below run when the MR is created; delete any that do not apply
(e.g. `/draft` once the MR is ready for review). -->

/draft
