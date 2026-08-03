#!/bin/sh
set -eu

confirm=${CONFIRM:-}
recover_tag=${RECOVER_TAG:-}
gh_bin=${GH_BIN:-gh}

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

[ "$confirm" = release ] || fail 'confirm must equal release'

if [ -n "$recover_tag" ]; then
  printf '%s\n' "$recover_tag" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || fail 'recover tag must be vX.Y.Z'
  git rev-parse --verify "$recover_tag^{commit}" >/dev/null 2>&1 || fail "tag $recover_tag does not exist"
  if "$gh_bin" release view "$recover_tag" >/dev/null 2>&1; then
    fail "release $recover_tag already exists"
  fi
  previous=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$recover_tag^" 2>/dev/null || true)
  [ -n "$previous" ] || fail "no release precedes $recover_tag"
  git checkout --detach "$recover_tag" >/dev/null
  printf 'tag=%s\nrange=%s..%s\ncreate_tag=false\n' "$recover_tag" "$previous" "$recover_tag"
  exit 0
fi

previous=$(git describe --tags --abbrev=0 --match 'v[0-9]*' HEAD 2>/dev/null || true)
[ -n "$previous" ] || fail 'no previous semantic release tag found'
[ "$(git rev-list --count "$previous..HEAD")" -gt 0 ] || fail "no commits since $previous"
printf 'range=%s..HEAD\ncreate_tag=true\n' "$previous"
