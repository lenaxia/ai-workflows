#!/usr/bin/env bash
# salvage-verdict.sh — recover a review verdict that opencode posted as a
# plain issue comment instead of an official review.
#
# Historical failure class (LLMSafeSpaces#870 and friends): opencode runs,
# completes the review, but the model posts the review-shaped body
# (## Code Review … ### Verdict …) as an issue comment via the comment tool
# instead of creating an official review via the reviews API. The /merge gate
# (and the verify step in pr-review.yml) require an OFFICIAL
# APPROVED/CHANGES_REQUESTED review pinned to the head SHA, so those PRs spun
# through extra rounds with no official verdict on record.
#
# This script finds the NEWEST review-shaped bot comment on a PR, derives the
# verdict (APPROVE -> APPROVED, REQUEST CHANGES -> CHANGES_REQUESTED) from its
# ### Verdict line, strips any stale "**Commit reviewed:**" line, and re-posts
# it as an official review pinned to the given head SHA.
#
# Exit status 0 + salvaged=true: an official review was created.
# Exit status 0 + salvaged=false: nothing to salvage (no review-shaped
# comment, or verdict unparseable). Callers should then fail visibly so the
# job surfaces the missing verdict (the pr-review.yml verify step).
#
# Usage:
#   REPOSITORY=owner/repo PR_NUMBER=NNN PR_HEAD_SHA=<sha> GH_TOKEN=<token> salvage-verdict.sh
#   # sets GITHUB_OUTPUT salvaged=true|false when GITHUB_OUTPUT is set

set -euo pipefail

: "${REPOSITORY:?REPOSITORY is required}"
: "${PR_NUMBER:?PR_NUMBER is required}"
: "${PR_HEAD_SHA:?PR_HEAD_SHA is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"

body="$(gh api "repos/${REPOSITORY}/issues/${PR_NUMBER}/comments" --paginate \
  --jq '[.[] | select(.user.login == "github-actions[bot]") | .body] | map(select(test("## Code Review") and test("### Verdict"))) | last // empty')"

if [ -z "${body}" ]; then
  echo "No review-shaped comment to salvage."
  [ -n "${GITHUB_OUTPUT:-}" ] && echo "salvaged=false" >> "${GITHUB_OUTPUT}"
  exit 0
fi

event="$(printf '%s\n' "${body}" | grep -oE '\*\*?(APPROVE|REQUEST CHANGES)\*\*?|\b(APPROVE|REQUEST CHANGES)\b' | grep -oE '(APPROVE|REQUEST CHANGES)' | head -1 || true)"

case "${event}" in
  APPROVE) event="APPROVED" ;;
  "REQUEST CHANGES") event="CHANGES_REQUESTED" ;;
  *)
    echo "Dumped verdict has no parseable APPROVE/REQUEST CHANGES verdict — not salvaging." >&2
    [ -n "${GITHUB_OUTPUT:-}" ] && echo "salvaged=false" >> "${GITHUB_OUTPUT}"
    exit 0
    ;;
esac

# Strip any stale "**Commit reviewed:**" line — the review's commit_id is set
# explicitly on the API call below, and a stale SHA in the body would
# contradict the pinned commit.
body="$(printf '%s\n' "${body}" | sed '/^\*\*Commit reviewed:\*\*/d')"

gh api "repos/${REPOSITORY}/pulls/${PR_NUMBER}/reviews" \
  -f commit_id="${PR_HEAD_SHA}" \
  -f event="${event}" \
  -f body="${body}" >/dev/null

[ -n "${GITHUB_OUTPUT:-}" ] && echo "salvaged=true" >> "${GITHUB_OUTPUT}"
echo "Salvaged dumped verdict as official ${event} review against HEAD ${PR_HEAD_SHA}."
