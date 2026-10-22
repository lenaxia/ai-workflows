#!/usr/bin/env bash
# check-verdict.sh — single source of truth for "was a review verdict
# delivered against this PR's head SHA?" Used by BOTH gates of the
# reusable pr-review workflow (Check verdict delivered / Verify review
# submitted) and the /review tail of ai-comment.yml, replacing the
# previously triplicated inline jq.
#
# Two-channel delivery (owner ruling ai-workflows#6209846676, issue #46):
#
#   review   — an official APPROVED/CHANGES_REQUESTED review by
#              github-actions[bot] pinned to the head SHA (the historical
#              gate), OR
#   comment  — a github-actions[bot] PR comment carrying the marker
#              <!-- ai-workflows-verdict: APPROVE|REQUEST_CHANGES sha=<sha> -->
#              whose sha EQUALS the head SHA. This is the REQUIRED fallback
#              for the cell where GitHub refuses the review event itself
#              (HTTP 422 "GitHub Actions is not permitted to approve pull
#              requests" — the repo/org setting class): the verdict comment
#              is the authoritative signal consumers already parse
#              (## Code Review / ### Verdict / **Commit reviewed:**).
#
# delivered=false is reserved for "neither channel could post" — the
# loud-red cell. A stale-sha marker or a stale review NEVER counts, and a
# review-shaped comment WITHOUT the marker does not count either (that is
# the "dumped verdict" class — salvage-verdict.sh owns upgrading it to an
# official review; gates re-run this script fresh afterwards).
#
# Channel-resolution lines on stdout are deliberate: they are the
# production proof that both channels were evaluated.
#
# Output (GITHUB_OUTPUT when set): delivered=review|comment|false
# Exit status: 0 whenever a decision was reached; a hard API failure exits
# non-zero (callers mark the step continue-on-error so a transient blip
# degrades to the verify gate's own fresh run, never a silent pass).
#
# Usage:
#   REPOSITORY=owner/repo PR_NUMBER=NNN PR_HEAD_SHA=<40-hex> GH_TOKEN=<token> check-verdict.sh

set -euo pipefail

: "${REPOSITORY:?REPOSITORY is required}"
: "${PR_NUMBER:?PR_NUMBER is required}"
: "${PR_HEAD_SHA:?PR_HEAD_SHA is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"

emit() {
  [ -n "${GITHUB_OUTPUT:-}" ] && echo "delivered=$1" >> "${GITHUB_OUTPUT}"
  echo "Verdict delivery channel: $1 (PR #${PR_NUMBER}, HEAD ${PR_HEAD_SHA})"
}

# --- First-pass duration (structural-failure gate for the bounded retry) -------
# Runtime drill-down (LLMSafeSpaces #1763 comment 6309172907): the observed
# retry rescue rate across all runs is 0%, and 42% of first-pass failures
# die in <60s (the structural class — startup asserts, quota 429 storms,
# provider-unavailable). Retrying those is pure waste (a quota-capped run
# retried inside its cooldown burned 81s for six more 429s, zero tools).
# Emit run_minutes so the workflow's retry step can require a run that
# actually lived long enough for a retry to plausibly help.
#
# SEMANTICS (deliberate approximation, pinned by
# TestCheckVerdict_RunMinutesMeasuresFromJobStart): run_minutes measures
# JOB elapsed time (now - the first "review"-named job's started_at), not
# the "Run OpenCode" step alone — it includes checkout (~18s median),
# preflight, prompt build, and cache restore. A structurally-doomed first
# pass therefore needs to die >3min minus that overhead before the gate
# bites; the residual overlap is accepted because the gate's premise is a
# 0% observed rescue rate (fail direction: a too-generous retry wastes one
# bounded pass, never loses a verdict). Two known edges: `date -d` is
# GNU-coreutils (the runner fleet is Linux-only; a BSD date would fail
# open to "no run_minutes"), and a clock-skewed FUTURE started_at yields a
# negative run_minutes, which fails CLOSED (retry skipped) — pinned
# visibly rather than silently.
#
# Fail-open contract: if the job lookup or date parse fails, emit nothing
# and the retry proceeds as before (run_minutes unset => gate's ''
# disjunct).
if [ -n "${GITHUB_OUTPUT:-}" ] && [ -n "${GITHUB_RUN_ID:-}" ]; then
  job_started="$(gh api "repos/${REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/jobs" \
    --jq '[.jobs[] | select(.name | test("review"))][0].started_at' 2>/dev/null)" || job_started=""
  if [ -n "${job_started}" ]; then
    now_epoch="$(date -u +%s)"
    started_epoch="$(date -u -d "${job_started}" +%s 2>/dev/null)" || started_epoch=""
    if [ -n "${started_epoch}" ]; then
      run_minutes=$(( (now_epoch - started_epoch) / 60 ))
      echo "run_minutes=${run_minutes}" >> "${GITHUB_OUTPUT}"
      echo "First pass has lived ${run_minutes}m (structural-failure gate for the retry)."
    fi
  fi
fi

# --- Channel 1: official review ------------------------------------------------
# --paginate + per-page count aggregation: /pulls/N/reviews pages at 30,
# oldest first, so a busy PR's head-SHA verdict can sit beyond page 1.
count="$(gh api "repos/${REPOSITORY}/pulls/${PR_NUMBER}/reviews" --paginate \
  --jq '[.[] | select(.user.login == "github-actions[bot]") | select((.state == "APPROVED" or .state == "CHANGES_REQUESTED") and .commit_id == "'"${PR_HEAD_SHA}"'")] | length' \
  | awk '{s+=$1} END {print s+0}')"

if [ "${count}" -gt 0 ]; then
  echo "Channel review: ${count} official bot verdict(s) against HEAD."
  emit review
  exit 0
fi
echo "Channel review: no official bot verdict against HEAD — checking comment fallback channel."

# --- Channel 2: marker verdict comment ------------------------------------------
# Newest-agnostic existence check across ALL pages (--paginate --slurp;
# gh rejects --slurp with --jq, so selection runs in an external jq pipe —
# same contract as salvage-verdict.sh). The marker must carry the FULL head
# SHA; a short or differing sha is a stale verdict and never counts.
marker="$(gh api "repos/${REPOSITORY}/issues/${PR_NUMBER}/comments" --paginate --slurp \
  | jq -r --arg sha "${PR_HEAD_SHA}" '
    [.[] | .[]
      | select(.user.login == "github-actions[bot]")
      | .body
      | select(test("<!-- ai-workflows-verdict: (APPROVE|REQUEST_CHANGES) sha=[0-9a-f]{40} -->"))
      | capture("ai-workflows-verdict: (?:APPROVE|REQUEST_CHANGES) sha=(?<sha>[0-9a-f]{40})")
      | select(.sha == $sha)]
    | length')"

if [ "${marker}" -gt 0 ]; then
  echo "Channel comment: ${marker} verdict-marker comment(s) against HEAD (official review channel refused or unavailable)."
  emit comment
  exit 0
fi

echo "Channel comment: no sha-matching verdict marker found."
emit false
