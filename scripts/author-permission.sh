#!/usr/bin/env bash
# author-permission.sh — pre-flight for the reusable pr-review workflow:
# decide BEFORE the LLM runs whether the opencode CLI's unconditional
# assertPermissions() will accept this PR's author.
#
# Why: `opencode github run` treats pull_request as a USER event and, for
# every user event, calls assertPermissions() (packages/opencode/src/cli/cmd
# /github.ts in the pinned action): repos.getCollaboratorPermissionLevel(
# owner, repo, username: actor) must return admin|write or the run dies
# before the LLM starts (~23s; the bounded retry then dies identically).
# For pull_request events the actor is the PR AUTHOR, and app-bot accounts
# structurally return 'none' (apps are not collaborators), as do outside
# contributors. Skipping the doomed attempts turns a confusing double
# death + bare "no verdict delivered" into an instant, explained failure.
#
# SAME-SOURCE CONTRACT (owner ruling ai-workflows#6209918061 point 3): this
# script queries EXACTLY the endpoint the CLI's assertPermissions uses —
# GET /repos/{owner}/{repo}/collaborators/{username}/permission — and
# accepts EXACTLY the same pass-set (admin|write). Never consult any
# divergent source (commenter association, org membership APIs, …) here: a
# divergent source could allow a run the CLI then refuses (two doomed LLM
# attempts) or block a run the CLI would have accepted.
#
# FAIL-OPEN: if the permission source itself errors (transient 5xx), emit
# author_allowed=true and let the CLI's own assert decide — the workflow
# tolerates the CLI failure (continue-on-error) and its verify gate fails
# loudly, so a flaky pre-flight can never silently skip a review.
#
# Output (GITHUB_OUTPUT when set): author_allowed=true|false
# Exit status: 0 whenever a decision was reached.
#
# Usage:
#   REPOSITORY=owner/repo PR_AUTHOR=<login> GH_TOKEN=<token> [TRUSTED_APP_BOTS='["bot[bot]"]'] author-permission.sh

set -euo pipefail

: "${REPOSITORY:?REPOSITORY is required}"
: "${PR_AUTHOR:?PR_AUTHOR is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"

# Always-trusted app bots (this pipeline's own propagation bot) — see
# scripts/default-trusted-bots.sh. The caller list EXTENDS this set.
. "$(dirname "$0")/default-trusted-bots.sh"

emit() {
  [ -n "${GITHUB_OUTPUT:-}" ] && echo "author_allowed=$1" >> "${GITHUB_OUTPUT}"
  echo "PR author ${PR_AUTHOR} pre-flight: author_allowed=$1"
}

# Trusted app-bot arm: authors on the caller's trusted_app_bots list OR the
# default-trusted set run with the CLI assert BYPASSED (TOKEN mode — see
# trusted-bot-auth.sh), so the collaborator lookup below would wrongly
# classify them as doomed and re-create exactly the gap the input exists to
# close. Admit them here and say why. Fail-closed: missing/malformed list
# JSON resolves to not-trusted and falls through to the same-source
# collaborator check.
if printf '%s' "${TRUSTED_APP_BOTS:-[]}" | jq -e --arg author "${PR_AUTHOR}" --argjson defaults "${DEFAULT_TRUSTED_APP_BOTS}" 'index($author) != null or ($defaults | index($author) != null)' >/dev/null 2>&1; then
  echo "PR author ${PR_AUTHOR} is trusted (default-trusted set or the caller's trusted_app_bots list) — opencode runs in TOKEN mode (assert bypassed for this actor only)."
  emit true
  exit 0
fi

# Percent-encode the login for the URL path (bot logins contain [ ]).
author_enc="$(printf '%s' "${PR_AUTHOR}" | jq -Rr @uri)"

permission="$(gh api "repos/${REPOSITORY}/collaborators/${author_enc}/permission" --jq '.permission' 2>/dev/null)" || {
  echo "WARN: permission lookup failed for ${PR_AUTHOR} (transient API error) — failing OPEN; the CLI's assertPermissions remains the enforcement." >&2
  emit true
  exit 0
}

echo "PR author ${PR_AUTHOR} collaborator permission: ${permission}"
case "${permission}" in
  "admin"|"write")
    emit true
    ;;
  *)
    echo "PR author ${PR_AUTHOR} is not a collaborator (permission=${permission}) — the opencode CLI's assertPermissions will refuse this run. Skipping the LLM attempts; a member-triggered /review is the recovery path." >&2
    emit false
    ;;
esac
