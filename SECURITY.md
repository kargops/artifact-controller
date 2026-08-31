# Security

## Reporting

Report vulnerabilities privately via
[GitHub security advisories](https://github.com/kargops/artifact-controller/security/advisories/new),
or by mail to idan.c@protonmail.com. Please do not open public issues for
security reports. You should hear back within a week.

## Supported versions

Pre-1.0: only the latest release receives fixes.

## Design notes relevant to security posture

- The controller's ServiceAccount is read-only against stores; generator runs
  use a separate identity that writes. A reconciler bug cannot delete or
  overwrite artifacts.
- Store credentials are resolved only from the controller's own namespace —
  `ArtifactClass` is cluster-scoped, so this is what prevents anyone who can
  write a class from exfiltrating arbitrary secrets.
- The controller ships no wildcard RBAC; generator-engine access is granted
  per engine via aggregation-labelled ClusterRoles.
- Auth failures from token endpoints are reported by status code only, never
  by echoing response bodies (which reflect client ids and sometimes more).
