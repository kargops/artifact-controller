#!/usr/bin/env bash
# Test entrypoint for CI (see .gitlab-ci.yml) and for reproducing CI locally.
#
# Beyond running the suite, this asserts that generated artifacts (deepcopy
# code, CRDs, RBAC) are committed and up to date — otherwise a stale CRD in
# git silently diverges from the Go types.
set -euo pipefail

cd "$(dirname "$0")/.."

# The CI test-script component invokes this as `bash -lc`, and a login shell
# sources /etc/profile, which on Debian *overwrites* PATH — dropping
# /usr/local/go/bin that the golang image sets via ENV. Put the toolchain back
# before anything needs it.
if ! command -v go >/dev/null 2>&1; then
  for candidate in "${GOROOT:-}/bin" /usr/local/go/bin /usr/lib/go/bin; do
    if [ -x "${candidate}/go" ]; then
      export PATH="${candidate}:${PATH}"
      break
    fi
  done
fi
if ! command -v go >/dev/null 2>&1; then
  echo "❌ no go toolchain on PATH ($PATH)"
  exit 1
fi

# Git refuses to operate on a tree owned by another user; the runner's
# init-permissions step can leave the checkout that way.
if [ -n "${CI:-}" ]; then
  git config --global --add safe.directory "$PWD" || true
fi

echo "==> toolchain"
go version

echo "==> generated artifacts are up to date"
make generate manifests
# Only the paths controller-gen writes, so unrelated work in api/ or config/
# does not masquerade as stale generated output.
generated=(
  api/v1alpha1/zz_generated.deepcopy.go
  config/crd/bases
  config/rbac/role.yaml
  charts/artifact-controller/crds
  charts/artifact-controller/templates/manager-clusterrole.yaml
)
if ! git diff --exit-code -- "${generated[@]}"; then
  echo
  echo "❌ Generated files are out of date. Run 'make generate manifests' and commit the result."
  exit 1
fi

# On a release tag, the chart must describe the thing being released: the
# stable publish job takes its version straight from Chart.yaml.
if [ -n "${CI_COMMIT_TAG:-}" ]; then
  echo "==> chart version matches tag $CI_COMMIT_TAG"
  want="${CI_COMMIT_TAG#v}"
  chart_version="$(awk '/^version:/ {print $2; exit}' charts/artifact-controller/Chart.yaml)"
  app_version="$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2; exit}' charts/artifact-controller/Chart.yaml)"
  if [ "$chart_version" != "$want" ] || [ "$app_version" != "$CI_COMMIT_TAG" ]; then
    echo "❌ Chart.yaml is out of step with the tag:"
    echo "   version=$chart_version (want $want), appVersion=$app_version (want $CI_COMMIT_TAG)"
    exit 1
  fi
fi

echo "==> gofmt"
unformatted="$(gofmt -l ./api ./cmd ./internal)"
if [ -n "$unformatted" ]; then
  echo "❌ Not gofmt'd:"
  echo "$unformatted"
  exit 1
fi

echo "==> vet"
go vet ./...

# The CI validate-chart job lints the chart in its own image; locally this runs
# only if helm happens to be installed.
if command -v helm >/dev/null 2>&1; then
  echo "==> helm lint"
  make helm-lint
else
  echo "==> helm lint (skipped: helm not installed)"
fi

# The envtest integration suite needs the kube-apiserver/etcd binaries that
# setup-envtest downloads. Where that download is unreachable, set
# SKIP_ENVTEST=1: the suite skips itself cleanly when KUBEBUILDER_ASSETS is
# unset, so unit tests still run and still gate the pipeline.
if [ "${SKIP_ENVTEST:-0}" = "1" ]; then
  echo "==> tests (SKIP_ENVTEST=1: unit tests only, integration suite skipped)"
  go test ./... -count=1
else
  echo "==> tests (unit + envtest integration suite)"
  make test
fi

echo "✅ all checks passed"
