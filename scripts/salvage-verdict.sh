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
# This script finds the NEWEST review-shaped bot comment on a PR whose
# "**Commit reviewed:**" line (when present) matches the given head SHA,
# derives the verdict (APPROVE -> APPROVED, REQUEST CHANGES ->
# CHANGES_REQUESTED) from its "### Verdict" section, and re-posts it as an
# official review pinned to the head SHA.
#
# Safety rules:
#   - Only github-actions[bot] comments are considered.
#   - A comment with a "**Commit reviewed:** <sha>" line that does NOT equal
#     the target head SHA is refused (a stale verdict must never be re-pinned
#     onto new commits) — the LLM retry then gets a chance to review properly.
#   - The verdict is parsed ONLY from the "### Verdict" section, so an
#     "APPROVE" mentioned in the Summary can never flip the event.
#   - A comment with no parseable verdict is NOT posted (a COMMENT-only
#     review would not satisfy the final verify gate and would add noise).
#
# Exit status 0 + salvaged=true: an official review was created.
# Exit status 0 + salvaged=false: nothing to salvage (no matching comment, or
# verdict unparseable, or stale SHA). Callers should then run the LLM retry /
# verify so the missing verdict surfaces visibly.
#
# salvaged=false is emitted BEFORE any network work so a transient failure
# inside this script still lets the caller's retry step proceed (the caller
# gates on salvaged != 'true').
#
# Usage:
#   REPOSITORY=owner/repo PR_NUMBER=NNN PR_HEAD_SHA=<sha> GH_TOKEN=<token> salvage-verdict.sh
#   # sets GITHUB_OUTPUT salvaged=true|false when GITHUB_OUTPUT is set

set -euo pipefail

: "${REPOSITORY:?REPOSITORY is required}"
: "${PR_NUMBER:?PR_NUMBER is required}"
: "${PR_HEAD_SHA:?PR_HEAD_SHA is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"

# Default to "nothing salvaged" before doing any work; flip to true only on
# a successful POST. This guarantees the caller (gated on salvaged != 'true')
# can proceed with its LLM retry even if a transient gh/api failure kills the
# script before it reaches the POST.
[ -n "${GITHUB_OUTPUT:-}" ] && echo "salvaged=false" >> "${GITHUB_OUTPUT}"

# Newest review-shaped bot comment, aggregated across ALL paginated pages.
# gh's --paginate emits one JSON array per page; --slurp wraps them into one
# outer array on stdout. gh REJECTS --slurp with --jq ("the `--slurp` option
# is not supported with `--jq` or `--template`", verified on gh 2.97.0), so
# the selection runs in an external jq -r pipe over the slurped pages. The
# flattened selection is therefore globally newest, never per-page.
body="$(gh api "repos/${REPOSITORY}/issues/${PR_NUMBER}/comments" --paginate --slurp \
  | jq -r '[.[] | .[] | select(.user.login == "github-actions[bot]") | .body | select(test("## Code Review") and test("### Verdict"))] | last // empty')"

if [ -z "${body}" ]; then
  echo "No review-shaped comment to salvage."
  exit 0
fi

# Freshness: a comment that declares a different reviewed SHA is a stale
# verdict from an earlier commit — refusing it is the ONLY safe choice, since
# re-pinning it would mark unreviewed commits as approved. The review prompt
# mandates the "**Commit reviewed:**" line, so the intended case always
# carries a matching SHA (or, pre-prompt-format, no line at all).
if printf '%s\n' "${body}" | grep -q '^\*\*Commit reviewed:\*\*'; then
  reviewed_sha="$(printf '%s\n' "${body}" | sed -n 's/^\*\*Commit reviewed:\*\*[[:space:]]*`\?\([0-9a-f]*\)`\?/\1/p' | head -1)"
  if [ -z "${reviewed_sha}" ] || [ "${reviewed_sha}" != "${PR_HEAD_SHA}" ]; then
    echo "Dumped verdict is for a different commit (${reviewed_sha:-unknown}) — refusing to re-pin onto HEAD ${PR_HEAD_SHA}." >&2
    exit 0
  fi
fi

# Verdict, parsed ONLY from the ### Verdict section (never from the Summary
# or any quoted output-format rules). _V is the running section flag: emit
# lines between "### Verdict" and the next "###" heading.
event="$(printf '%s\n' "${body}" | awk '
  /^### Verdict/ { in_verdict=1; next }
  /^### / { in_verdict=0 }
  in_verdict { print }
' | grep -oE '(APPROVE|REQUEST CHANGES)' | head -1 || true)"

case "${event}" in
  APPROVE) event="APPROVED" ;;
  "REQUEST CHANGES") event="CHANGES_REQUESTED" ;;
  *)
    echo "Dumped verdict has no parseable APPROVE/REQUEST CHANGES verdict in its ### Verdict section — not salvaging." >&2
    exit 0
    ;;
esac

# Strip the "**Commit reviewed:**" line — the review's commit_id is set
# explicitly on the API call below; a body line would be redundant but is
# stripped for cleanliness after the freshness check above passed.
body="$(printf '%s\n' "${body}" | sed '/^\*\*Commit reviewed:\*\*/d')"

gh api "repos/${REPOSITORY}/pulls/${PR_NUMBER}/reviews" \
  -f commit_id="${PR_HEAD_SHA}" \
  -f event="${event}" \
  -f body="${body}" >/dev/null

[ -n "${GITHUB_OUTPUT:-}" ] && echo "salvaged=true" >> "${GITHUB_OUTPUT}"
echo "Salvaged dumped verdict as official ${event} review against HEAD ${PR_HEAD_SHA}."
