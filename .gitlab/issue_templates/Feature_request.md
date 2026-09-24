<!-- Mirrors .github/ISSUE_TEMPLATE/feature_request.yml; ci/templates_test.go
keeps the two in step. GitLab templates cannot mark fields required — every
section below is required unless it says "optional".

This project is young (v1alpha1) and deliberately small. The best first
contribution is often an issue describing what you tried and where it
surprised you — that is a feature request in its most useful form.

Requesting a new store driver? Think twice first: the `http` driver plus an
ArtifactClass covers most stores without a controller release. A dedicated
driver earns its place when the store needs bespoke signing or semantics CEL
cannot express. -->

## The problem

<!-- What you are trying to do, and where the current behavior stops you.
Concrete beats abstract — a real ArtifactClass you wish you could write says
more than a paragraph. -->

## Proposed shape (optional)

<!-- If you have a spec/API shape in mind, sketch it. YAML welcome. -->

```yaml
```

## Area

<!-- Keep the one that applies:
- api (CRDs, spec fields)
- controller (reconcile behavior)
- store (a driver)
- generator (run templating/interpretation)
- chart (Helm packaging)
- docs
-->

/label ~"type/feature"
