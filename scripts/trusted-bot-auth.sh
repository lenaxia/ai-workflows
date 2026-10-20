#!/usr/bin/env bash
# trusted-bot-auth.sh — decide whether `opencode github run` should execute in
# TOKEN mode for this event actor.
#
# Why: the opencode CLI's startup assertPermissions() calls
# repos.getCollaboratorPermissionLevel(owner, repo, username: actor) and
# refuses anything but admin|write. App-bot accounts (GitHub Apps) can NEVER
# be collaborators — the endpoint structurally returns 'none' for them — so
# any PR they author and any comment they post is refused before the LLM
# starts, even when the app's installation holds contents:write on the repo.
# The assert has exactly one bypass: it is skipped entirely when the TOKEN
# env var is set (useEnvGithubToken() returns process.env["TOKEN"]; the
# action's use_github_token input maps to USE_GITHUB_TOKEN, which nothing in
# the CLI consumes at the pinned ref — verified against
# anomalyco/opencode@0cf0294 github/index.ts lines 340-342/757-762).
#
# This script implements the opt-in caller trust decision: when the event
# actor's exact login is on the caller-provided trusted_app_bots list, export
# TOKEN=<the job's GITHUB_TOKEN> so the assert is bypassed FOR THAT ACTOR
# ONLY. Every other actor (all humans, unlisted bots) gets an EMPTY output,
# and an empty TOKEN is falsy in JS — the assert keeps enforcing exactly as
# before. Fail-closed: malformed or missing list JSON, or a jq failure,
# resolves to not-trusted.
#
# Security contract: the caller admission gate (the consumer's `if:` on
# comment events / the skip-list + this input on pull_request events) stays
# the ONLY authorization check — this script never admits anyone the caller
# did not explicitly list. Never admit wildcard or human logins through this
# list; each entry is a deliberate trust decision (mirrors the #1710 exact-
# login allowlist discipline in consumer callers).
#
# Output (GITHUB_OUTPUT when set): token=<GITHUB_TOKEN> when trusted, else
# token= (empty). Exit status: 0 whenever a decision was reached.
#
# Usage:
#   ACTOR=<login> TRUSTED_APP_BOTS='["bot[bot]"]' GH_TOKEN_UNUSED=x trusted-bot-auth.sh
# (GITHUB_TOKEN from the workflow env is exported as TOKEN only when trusted.)

set -euo pipefail

: "${ACTOR:?ACTOR is required}"

TRUSTED_APP_BOTS="${TRUSTED_APP_BOTS:-[]}"

emit() {
  [ -n "${GITHUB_OUTPUT:-}" ] && printf 'token=%s\n' "$1" >> "${GITHUB_OUTPUT}"
}

if ! printf '%s' "${TRUSTED_APP_BOTS}" | jq -e 'type == "array"' >/dev/null 2>&1; then
  echo "WARN: trusted_app_bots is not a JSON array — failing CLOSED (no TOKEN bypass)." >&2
  emit ""
  exit 0
fi

if printf '%s' "${TRUSTED_APP_BOTS}" | jq -e --arg actor "${ACTOR}" 'index($actor) != null' >/dev/null 2>&1; then
  echo "Actor ${ACTOR} is a trusted app bot — opencode runs in TOKEN mode (startup assert bypassed for this actor only)."
  emit "${GITHUB_TOKEN:-}"
  # TOKEN MUST ride GITHUB_ENV, not a caller step env: composite actions'
  # inner run steps that declare their own env: block (the opencode action's
  # "Run opencode" step does) receive ONLY job-level env + GITHUB_ENV vars —
  # the calling step's env is dropped (verified live: TOKEN:*** appeared in
  # the cache step's dump but never in the CLI step's, while an OPENCODE_PROMPT
  # exported via GITHUB_ENV reached the CLI — run 39373330689). Untrusted
  # actors write NOTHING here: an absent TOKEN is falsy and the assert
  # enforces exactly as before.
  if [ -n "${GITHUB_ENV:-}" ] && [ -n "${GITHUB_TOKEN:-}" ]; then
    printf 'TOKEN=%s\n' "${GITHUB_TOKEN}" >> "${GITHUB_ENV}"
  fi
  exit 0
fi

echo "Actor ${ACTOR} is not on the trusted_app_bots list — startup assert enforces (TOKEN unset)."
emit ""
exit 0
