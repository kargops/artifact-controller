#!/bin/bash
# Pre-warms the toolchain ./ci/test.sh needs, so the first gate an agent runs
# in a fresh Claude Code on the web container is fast and any proxy/network
# failure surfaces here with a clear message instead of mid-validation.
#
# Web-only: a local `claude` session already has these caches from prior runs.
# The go.sum guard mechanics are ported from the Roots project's hook, where
# they are themselves regression-tested.
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# `go mod download` silently rewrites go.sum to add missing checksum entries
# when the checked-out go.sum is incomplete for the current build list, and
# -mod=readonly does not prevent it (that flag guards go build/test, not this
# command, whose whole job is populating go.sum). A prewarm must never mutate
# a committed manifest silently: a "fixed" go.sum would hide that the
# committed one was wrong, and would land in the next commit unnoticed.
#
# Restore from a byte-exact snapshot rather than `git checkout -- go.sum`:
# SessionStart also fires on resume, where the tree can already carry
# uncommitted go.sum work (someone ran `go mod tidy` earlier in the session).
# A git-based restore would destroy it. Snapshotting is indifferent to whether
# the file started clean. The module cache stays warm either way — only the
# checksum file rolls back.
_GO_SUM_SNAPSHOT=""

_restore_go_sum() {
  [ -n "$_GO_SUM_SNAPSHOT" ] && [ -f "$_GO_SUM_SNAPSHOT" ] || return 0
  cmp -s go.sum "$_GO_SUM_SNAPSHOT" || cp "$_GO_SUM_SNAPSHOT" go.sum
  rm -f "$_GO_SUM_SNAPSHOT"
  _GO_SUM_SNAPSHOT=""
}

prewarm_go_modules() {
  local mutated=0 rc=0
  _GO_SUM_SNAPSHOT="$(mktemp)"
  cp go.sum "$_GO_SUM_SNAPSHOT"

  # Arm the rollback before the command starts: the SessionStart timeout or a
  # cancelled session can kill `go mod download` after checksums are written
  # but before any straight-line rollback below would run. Only a trap covers
  # that path — and it covers the ordinary returns too, so nothing below
  # copies the snapshot back by hand.
  trap _restore_go_sum EXIT INT TERM

  # `|| rc=$?`, not `if ! go mod download`: `!` inverts the status before the
  # branch body runs, so `$?` there is 0 and a real failure (module proxy
  # down, network blocked) would be reported as success.
  go mod download || rc=$?

  # Record whether the command touched go.sum before the restore erases the
  # evidence — a download that fails partway mutates the file exactly as a
  # successful one does, so this is asked independently of rc.
  cmp -s go.sum "$_GO_SUM_SNAPSHOT" || mutated=1

  _restore_go_sum
  trap - EXIT INT TERM

  if [ "$rc" -ne 0 ]; then
    return "$rc"
  fi

  if [ "$mutated" -eq 1 ]; then
    echo "go.sum is incomplete for the current build list: go mod download" >&2
    echo "rewrote it. Restored go.sum byte-for-byte (including any uncommitted" >&2
    echo "edits) and refusing to continue -- run 'go mod tidy' and commit." >&2
    return 1
  fi

  return 0
}

echo "==> go mod download"
prewarm_go_modules

# controller-gen / setup-envtest / kustomize install into bin/ on first use;
# the envtest kube-apiserver+etcd download is the single slowest first-gate
# item. Both write only under bin/ (gitignored), so no manifest guard needed.
echo "==> tool binaries (controller-gen, setup-envtest, kustomize)"
make controller-gen setup-envtest kustomize

echo "==> envtest binaries"
ENVTEST_K8S_VERSION="$(sed -n 's/^ENVTEST_K8S_VERSION ?= //p' Makefile)"
bin/setup-envtest use "$ENVTEST_K8S_VERSION" --bin-dir bin -p path >/dev/null
echo "session prewarm complete"
