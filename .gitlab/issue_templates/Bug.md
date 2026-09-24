<!-- Mirrors .github/ISSUE_TEMPLATE/bug_report.yml; ci/templates_test.go keeps
the two in step. GitLab templates cannot mark fields required — every section
below is required unless it says "optional".

Thanks — a precise report is often the best first contribution to this
project. The status/conditions output usually contains the answer, so please
include it even when it looks unremarkable.

Security vulnerability? Do not file it here — report it privately as
SECURITY.md describes. -->

## Store driver

<!-- Keep the one that applies:
- s3
- oci
- artifactory
- nexus
- ami
- http
- fake
- not driver-related
-->

## Controller version

<!-- Chart or image version (e.g. v0.9.1), or the commit if built from source. -->

## ArtifactClass (sanitized)

<!-- The class involved, with credentials/URLs redacted as needed. Keep the
store and generator blocks intact — most bugs live there. -->

```yaml
```

## Artifact and its observed status

<!-- `kubectl get artifact <name> -o yaml` — spec AND status. The conditions
and state are the controller's own account of what it did. -->

```yaml
```

## Expected vs observed

<!-- What you expected to happen, what actually happened, and how to
reproduce it. -->

## Controller logs (optional)

<!-- Relevant lines from the controller pod, if you have them. -->

```text
```

/label ~"type/bug"
