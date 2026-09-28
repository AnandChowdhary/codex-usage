#!/usr/bin/env bash
# Prints the next release tag (vX.Y.Z) based on the conventional commits
# since the last tag, or nothing if there's nothing to release.
#
#   feat: …                          minor
#   fix:, perf:, refactor:, anything  patch
#   type!: … or BREAKING CHANGE:      major (minor while below 1.0.0)
#   only docs/test/ci/chore/style/build commits: no release
#
# Usage: scripts/next-version.sh [auto|patch|minor|major]
set -euo pipefail

bump=${1:-auto}

# Already released (e.g. a later push was released first).
if [[ -n $(git tag --contains HEAD --list 'v[0-9]*') ]]; then
  exit 0
fi

last=$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' 2>/dev/null || true)
range=HEAD
if [[ -n $last ]]; then
  range="$last..HEAD"
else
  last=v0.0.0
fi

if [[ $bump == auto ]]; then
  subjects=$(git log --format=%s "$range")
  bodies=$(git log --format=%b "$range")
  type='[a-z]+(\([^)]*\))?'
  if [[ -z $subjects ]]; then
    exit 0
  elif grep -qE "^$type!:" <<<"$subjects" || grep -qE '^BREAKING[ -]CHANGE:' <<<"$bodies"; then
    bump=breaking
  elif grep -qE '^feat(\([^)]*\))?:' <<<"$subjects"; then
    bump=minor
  elif grep -qvE '^(docs|test|ci|chore|style|build)(\([^)]*\))?:' <<<"$subjects"; then
    bump=patch
  else
    exit 0
  fi
fi

IFS=. read -r major minor patch <<<"${last#v}"
case $bump in
  breaking)
    if ((major == 0)); then
      minor=$((minor + 1)) patch=0
    else
      major=$((major + 1)) minor=0 patch=0
    fi
    ;;
  major) major=$((major + 1)) minor=0 patch=0 ;;
  minor) minor=$((minor + 1)) patch=0 ;;
  patch) patch=$((patch + 1)) ;;
  *)
    echo "unknown bump: $bump" >&2
    exit 2
    ;;
esac
echo "v$major.$minor.$patch"
