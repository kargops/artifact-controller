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

## Verifying releases

Images and charts are signed keyless with [cosign](https://docs.sigstore.dev)
by the release workflow's OIDC identity, and the image carries an SPDX SBOM
attestation (also attached to each GitHub release):

The identity is pinned to the release workflow at a tag — a signature from any
other workflow or ref does not verify. Image signatures use cosign v3's bundle
format, so verifying them needs cosign v3+:

```sh
cosign verify ghcr.io/kargops/artifact-controller:vX.Y.Z \
  --certificate-identity-regexp '^https://github\.com/kargops/artifact-controller/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

cosign verify-attestation --type spdxjson ghcr.io/kargops/artifact-controller:vX.Y.Z \
  --certificate-identity-regexp '^https://github\.com/kargops/artifact-controller/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

SBOMs are per-architecture (syft inventories one platform per scan); the
attestation on the multi-arch index carries the amd64 document, and each
architecture's manifest digest carries its own.

The chart at `oci://ghcr.io/kargops/charts/artifact-controller` is signed the
same way (Artifact Hub shows this as the signed badge).
