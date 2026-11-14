#!/usr/bin/env bash
# review-state.sh — deterministic read-side facts for the review gate
# (docs/review-gate-v1-spec.md §2). The reviewer never polls CI or counts its
# own rounds; the workflow injects these numbers, and anything the reviewer
# CLAIMS about them is validated here. Subcommands:
#
#   rounds <pr>                    → integer: prior bot verdicts for the PR's
#     current head = formal reviews by the bot actor whose body carries the
#     `**Commit reviewed:**` line AND whose commit_id == head, PLUS
#     sha-pinned verdict-marker comments
#     (<!-- ai-workflows-verdict: APPROVE|REQUEST_CHANGES sha=<40-hex> -->
#     with sha == head). Stale-sha markers and stale reviews NEVER count —
#     a push resets the verdict ledger for the new head (mirrors
#     check-verdict.sh's channel freshness), so the round budget measures
#     demand-generation against the head actually being merged. Marker
#     regex/jq is byte-identical to check-verdict.sh's — one definition of
#     "a verdict", not two.
#
#   ci-status <sha>                → prints ci=success|failure|pending|none
#     over ALL runs associated with the sha: any failed/cancelled/timed-out
#     run → failure (fail-closed: a cancelled check is not a green check);
#     zero runs → none; any still running/queued/unconcluded → pending;
#     else success. Non-failure terminal odds and ends (skipped/neutral)
#     count as concluded.
#
#   head-check <sha> [--pr N]      → exit 0 iff the PR's current head sha ==
#     <sha>; prints head-check=ok on match, a mismatch line on stderr and
#     exits 1 otherwise. The PR comes from --pr or env PR_NUMBER (the merge
#     gate runs inside the workflow, where PR_NUMBER is always set).
#
# Common flags: --repo R (default env REPOSITORY, the check-verdict.sh
# convention). The bot actor is env-overridable: REVIEW_BOT_ACTOR
# (default github-actions[bot], same actor check-verdict.sh pins).
#
# Fixture mode for golden tests (no gh, no token, no network):
#   REVIEW_STATE_FROM_FILE=<dir> (or --from-file <dir>) reads each gh call's
#   JSON verbatim from the named file instead of the network:
#
#     pull.json     — GET repos/R/pulls/N              (object; reads .head.sha)
#     reviews.json  — GET repos/R/pulls/N/reviews      (ARRAY, all pages merged)
#     comments.json — GET repos/R/issues/N/comments    (ARRAY, all pages merged)
#     runs.json     — GET repos/R/actions/runs?head_sha=… (object with
#                     .workflow_runs[]; a bare array is also accepted)
#
#   A missing file is treated as an empty result. The <pr>/<sha> arguments
#   are NOT re-validated against the fixtures — tests author the exact world
#   they assert. Exit codes: 0 = answered, 1 = resolution failure (e.g.
#   head-check mismatch, unresolvable PR head), 2 = usage error.
#
# Usage:
#   review-state.sh rounds <pr> [--repo R]
#   review-state.sh ci-status <sha> [--repo R]
#   review-state.sh head-check <sha> [--pr N] [--repo R]

set -euo pipefail

BOT="${REVIEW_BOT_ACTOR:-github-actions[bot]}"
FIXTURE="${REVIEW_STATE_FROM_FILE:-}"
REPO="${REPOSITORY:-}"
PR="${PR_NUMBER:-}"

usage() {
  cat >&2 <<'EOF'
usage: review-state.sh rounds <pr> [--repo R]
       review-state.sh ci-status <sha> [--repo R]
       review-state.sh head-check <sha> [--pr N] [--repo R]
env: REPOSITORY (default repo), PR_NUMBER (head-check), REVIEW_BOT_ACTOR,
     REVIEW_STATE_FROM_FILE=<dir> (fixture mode: pull.json reviews.json
     comments.json runs.json)
EOF
  exit 2
}

SUB=""
declare -a POSITIONAL=()
while [ $# -gt 0 ]; do
  case "$1" in
    --repo)     [ $# -ge 2 ] || usage; REPO="$2"; shift 2 ;;
    --repo=*)   REPO="${1#*=}"; shift ;;
    --pr)       [ $# -ge 2 ] || usage; PR="$2"; shift 2 ;;
    --pr=*)     PR="${1#*=}"; shift ;;
    --from-file) [ $# -ge 2 ] || usage; FIXTURE="$2"; shift 2 ;;
    --from-file=*) FIXTURE="${1#*=}"; shift ;;
    -h|--help)  usage ;;
    *) if [ -z "$SUB" ]; then SUB="$1"; else POSITIONAL+=("$1"); fi; shift ;;
  esac
done

[ -n "$SUB" ] || usage
case "$SUB" in
  rounds|ci-status|head-check) ;;
  *) usage ;;
esac
[ "${#POSITIONAL[@]}" -eq 1 ] || usage
TARGET="${POSITIONAL[0]}"
[ -n "$REPO" ] || { printf 'review-state.sh: --repo or REPOSITORY is required\n' >&2; exit 2; }

# fetch_pages <route> <fixture-name> — emits one JSON document per page on
# stdout. Fixture mode: the file verbatim (it already holds the FULL
# aggregated result = one logical page). Live mode: gh --paginate streams
# pages raw; consumers apply their per-page jq filter to each document, the
# same aggregation shape check-verdict.sh uses (gh rejects --slurp with
# --jq, so per-document filtering is the portable idiom).
fetch_pages() {
  if [ -n "$FIXTURE" ]; then
    if [ -f "$FIXTURE/$2" ]; then cat "$FIXTURE/$2"; fi
    return 0
  fi
  gh api "$1" --paginate
}

# fetch_one <route> <fixture-name> — single-document route (the pull object);
# fixture default is {} so callers can null-guard uniformly.
fetch_one() {
  if [ -n "$FIXTURE" ]; then
    if [ -f "$FIXTURE/$2" ]; then cat "$FIXTURE/$2"; else printf '{}\n'; fi
    return 0
  fi
  gh api "$1"
}

pull_head_sha() { # pull_head_sha <pr>
  fetch_one "repos/$REPO/pulls/$1" "pull.json" \
    | jq -r 'if type == "object" then (.head.sha // "") else "" end'
}

case "$SUB" in
  rounds)
    HEAD_SHA="$(pull_head_sha "$TARGET")"
    if [ -z "$HEAD_SHA" ]; then
      printf 'review-state.sh: cannot resolve head sha for PR %s\n' "$TARGET" >&2
      exit 1
    fi
    # Formal reviews: bot actor + Commit-reviewed line + pinned to THIS head.
    rev_count=$(fetch_pages "repos/$REPO/pulls/$TARGET/reviews?per_page=100" "reviews.json" \
      | jq --arg bot "$BOT" --arg sha "$HEAD_SHA" \
          '[.[] | select((.user.login // "") == $bot)
                     | select((.commit_id // "") == $sha)
                     | select((.body // "") | contains("**Commit reviewed:**"))] | length' \
      | awk '{s+=$1} END {print s+0}')
    # Marker comments: the check-verdict.sh marker, sha must equal the head.
    mrk_count=$(fetch_pages "repos/$REPO/issues/$TARGET/comments?per_page=100" "comments.json" \
      | jq --arg bot "$BOT" --arg sha "$HEAD_SHA" \
          '[.[] | select((.user.login // "") == $bot)
                   | (.body // "")
                   | select(test("<!-- ai-workflows-verdict: (APPROVE|REQUEST_CHANGES) sha=[0-9a-f]{40} -->"))
                   | capture("ai-workflows-verdict: (?:APPROVE|REQUEST_CHANGES) sha=(?<sha>[0-9a-f]{40})")
                   | select(.sha == $sha)] | length' \
      | awk '{s+=$1} END {print s+0}')
    echo "$((rev_count + mrk_count))"
    ;;

  ci-status)
    # One conclusion token per run: null conclusion (running/queued) maps to
    # RUNNING. cancelled/timed_out/action_required/startup_failure are
    # failure-shaped — a merge gate may not read an aborted run as green.
    # The tokens are captured via command substitution (NOT a process
    # substitution) so set -e -o pipefail turns a gh outage or a malformed
    # fixture into a loud non-zero exit instead of a silent ci=none.
    fail=0; run=0; any=0
    concls="$(fetch_pages "repos/$REPO/actions/runs?head_sha=$TARGET&per_page=100" "runs.json" \
      | jq -r '(.workflow_runs? // .)[]? | (.conclusion // "RUNNING")')"
    while IFS= read -r concl; do
      [ -n "$concl" ] || continue
      any=1
      case "$concl" in
        failure|startup_failure|timed_out|action_required|cancelled) fail=1 ;;
        RUNNING) run=1 ;;
      esac
    done <<< "$concls"
    if [ "$any" -eq 0 ]; then
      echo "ci=none"
    elif [ "$fail" -eq 1 ]; then
      echo "ci=failure"
    elif [ "$run" -eq 1 ]; then
      echo "ci=pending"
    else
      echo "ci=success"
    fi
    ;;

  head-check)
    [ -n "$PR" ] || { printf 'review-state.sh: head-check needs --pr or PR_NUMBER\n' >&2; exit 2; }
    CURRENT="$(pull_head_sha "$PR")"
    if [ "$CURRENT" = "$TARGET" ]; then
      echo "head-check=ok"
      exit 0
    fi
    printf 'head-check=mismatch expected=%s actual=%s\n' "$TARGET" "$CURRENT" >&2
    exit 1
    ;;
esac
