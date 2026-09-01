---
description: Apply only the human-approved subset of review findings to a PR
argument-hint: <PR number and the approved finding list>
---

Apply the approved findings to: $ARGUMENTS

Do not implement any finding not explicitly listed as approved here — not your
own earlier findings, not another reviewer's, however correct they look. If the
approved list is ambiguous, stop and ask.

## For each approved finding

1. The smallest valid correction — no drive-by refactoring around it.
2. A focused regression test that fails without the fix and passes with it.
   Exception: docs/workflow-prose findings get covered at the lowest practical
   layer instead — do not invent a brittle test that just greps for prose.

## When done

- Run the validation ladder once: focused tests, then `./ci/test.sh`.
- Land the whole round as **one** push, not one push per finding — every push
  starts a fresh CI run. Exception: a fix whose entire purpose is to see what
  CI says about it goes on its own.
- If a finding conflicts with the approved approach, flag it rather than
  resolving it yourself.
- Do not resolve review threads, undraft, or merge.
