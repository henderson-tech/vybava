#!/usr/bin/env bash
# One-command release: scripts/release.sh [major|minor|patch]  (default: minor)
# Computes the next tag from the latest v*, sanity-checks the checkout, tags,
# pushes, and watches the Release workflow that publishes archives + the cask.
set -euo pipefail

bump="${1:-minor}"
case "$bump" in major|minor|patch) ;; *) echo "usage: $0 [major|minor|patch]" >&2; exit 2 ;; esac

branch=$(git rev-parse --abbrev-ref HEAD)
[ "$branch" = "main" ] || { echo "release from main only (on $branch)" >&2; exit 1; }
[ -z "$(git status --porcelain)" ] || { echo "working tree not clean" >&2; exit 1; }
git fetch origin --tags -q
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || { echo "main not in sync with origin/main" >&2; exit 1; }

latest=$(git tag --list 'v*' --sort=-v:refname | head -1)
latest="${latest:-v0.0.0}"
IFS=. read -r major minor patch <<<"${latest#v}"
case "$bump" in
  major) next="v$((major + 1)).0.0" ;;
  minor) next="v${major}.$((minor + 1)).0" ;;
  patch) next="v${major}.${minor}.$((patch + 1))" ;;
esac

if command -v goreleaser >/dev/null; then goreleaser check; else echo "note: goreleaser not installed, skipping config check"; fi

echo "releasing ${latest} -> ${next} at $(git rev-parse --short HEAD)"
git tag -a "$next" -m "Release $next"
git push origin "$next"

echo "watching the Release workflow…"
# GitHub registers the tag-triggered run seconds after the push; poll ~2 min for it.
run_id=""
for _ in {1..24}; do
  sleep 5
  run_id=$(gh run list --workflow Release --branch "$next" --limit 1 --json databaseId --jq '.[0].databaseId // empty')
  if [ -n "$run_id" ]; then break; fi
done
if [ -z "$run_id" ]; then
  echo "no Release run for $next appeared within 2 min; $next is pushed — watch it with: gh run list --workflow Release --branch $next" >&2
  exit 1
fi
gh run watch "$run_id" --exit-status
echo "released $next — upgrade with: brew upgrade --cask vybava"
