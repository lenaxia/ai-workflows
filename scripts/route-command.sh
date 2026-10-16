#!/usr/bin/env bash
# route-command.sh — GitHub Actions AI-command routing logic (single source of truth).
#
# Sourced by .github/workflows/ai-comment.yml's "Build prompt" step and exercised
# by tests/gharouter/route_test.go. Keeping the routing in one sourced script means
# the routing/hold-detection surface has a persistent regression guard instead of
# being untestable inline YAML.
#
# Inputs (read from the environment):
#   COMMENT_BODY  — the triggering issue/PR comment body
#   PR_URL        — github.event.issue.pull_request.url (non-empty when on a PR)
#   EVENT_NAME    — github.event_name (issue_comment | pull_request_review_comment)
#   OUT_FILE      — where to write the assembled prompt (default /tmp/opencode_prompt.txt)
#   PROMPTS_DIR   — directory containing context.md, core-rules.md, *.md prompts
#                   (default .github/prompts, relative to the repo root)
#
# Outputs (set as shell variables after route_command returns):
#   COMMAND     — the resolved command token (e.g. /design, /merge, /ai)
#   NOTE        — the collaborator's trailing text (command + --no-merge stripped)
#   HOLD_MERGE  — 1 if a code-change command was held via --no-merge, else 0
#   OUT_FILE    — the prompt file path that was written
#
# When executed directly (not sourced), runs route_command and prints the
# resolved vars as KEY=VALUE lines for ad-hoc testing/inspection.

set -euo pipefail

route_command() {
  local comment_body="${COMMENT_BODY:-}"
  local pr_url="${PR_URL:-}"
  local event_name="${EVENT_NAME:-}"
  local prompts_dir="${PROMPTS_DIR:-.github/prompts}"
  local out_file="${OUT_FILE:-/tmp/opencode_prompt.txt}"

  CONTEXT_FILE="${prompts_dir}/context.md"
  CORE_FILE="${prompts_dir}/core-rules.md"
  WORKFLOW_FILE="${prompts_dir}/code-change-workflow.md"
  OUT="$out_file"
  OUT_FILE="$out_file"

  local pr_head_sha="${PR_HEAD_SHA:-}"

  # Verdict-delivery fallback directive (issue #46 / owner ruling
  # ai-workflows#6209846676): appended to every review-shaped prompt so the
  # reviewer never lets a completed verdict die on a platform refusal of the
  # review event (422 "GitHub Actions is not permitted to approve pull
  # requests"). The marker line is machine-greppable — scripts/check-verdict.sh
  # accepts it as the delivered verdict when the official review channel is
  # refused. Defined once, appended by the review paths below.
  local fallback_directive
  fallback_directive=$'\n\nVerdict-delivery contract (overrides any conflicting instruction above): FIRST always try to submit your verdict as a formal pull request review event (`gh pr review <N> --approve` or `--request-changes`) against the head SHA recorded in the prompt. If GitHub refuses the review submission itself — HTTP 422 / "GitHub Actions is not permitted to approve pull requests" / any "not permitted" refusal on the SUBMIT call (not on reads) — you MUST still deliver the complete review: post it as ONE ordinary PR comment whose first line is exactly `<!-- ai-workflows-verdict: APPROVE sha=<full head SHA from the prompt> -->` (or REQUEST_CHANGES), followed by the full review body in the required format (**Commit reviewed:** line, ## Code Review, all sections, ### Verdict). Never deliver nothing. Never submit a COMMENT-only review, a dishonest verdict, or a verdict pinned to any other SHA.\n'

  # Detect which command was used (check start of comment first, then inline).
  # Each pattern requires the command keyword followed by end-of-string or
  # a space — this prevents false matches like /testing → /test or /fixing → /fix.
  COMMAND=""
  case "$comment_body" in
    /implement[[:space:]]*|/implement) COMMAND="/implement" ;;
    /security[[:space:]]*|/security)   COMMAND="/security" ;;
    /analyze[[:space:]]*|/analyze)     COMMAND="/analyze" ;;
    /review[[:space:]]*|/review)       COMMAND="/review" ;;
    /explain[[:space:]]*|/explain)     COMMAND="/explain" ;;
    /triage[[:space:]]*|/triage)       COMMAND="/triage" ;;
    /design[[:space:]]*|/design)       COMMAND="/design" ;;
    /merge[[:space:]]*|/merge)         COMMAND="/merge" ;;
    /test[[:space:]]*|/test)           COMMAND="/test" ;;
    /fix[[:space:]]*|/fix)             COMMAND="/fix" ;;
    /help[[:space:]]*|/help)           COMMAND="/help" ;;
    /ai[[:space:]]*|/ai)               COMMAND="/ai" ;;
    *[[:space:]]/implement[[:space:]]*) COMMAND="/implement" ;;
    *[[:space:]]/security[[:space:]]*)  COMMAND="/security" ;;
    *[[:space:]]/analyze[[:space:]]*)   COMMAND="/analyze" ;;
    *[[:space:]]/review[[:space:]]*)    COMMAND="/review" ;;
    *[[:space:]]/explain[[:space:]]*)   COMMAND="/explain" ;;
    *[[:space:]]/triage[[:space:]]*)    COMMAND="/triage" ;;
    *[[:space:]]/design[[:space:]]*)    COMMAND="/design" ;;
    *[[:space:]]/merge[[:space:]]*)     COMMAND="/merge" ;;
    *[[:space:]]/test[[:space:]]*)      COMMAND="/test" ;;
    *[[:space:]]/fix[[:space:]]*)       COMMAND="/fix" ;;
    *[[:space:]]/help[[:space:]]*)      COMMAND="/help" ;;
    *[[:space:]]/ai[[:space:]]*)        COMMAND="/ai" ;;
    *)               COMMAND="/ai" ;;
  esac

  NOTE="$(printf '%s' "$comment_body" | sed "s|.*${COMMAND}||" | sed 's/^[[:space:]]*//' | sed 's/[[:space:]]*$//')"

  # Detect the --no-merge hold flag. It's a global modifier:
  # strip it from NOTE for EVERY command (so it never pollutes the
  # description/topic), but only ACT on it for the four auto-merging
  # code-change commands. `/design` always holds (enforced in its prompt);
  # `/merge` is the explicit merge and ignores the flag.
  #
  # TRAILING-ONLY: --no-merge is recognized as the flag ONLY when it is the
  # last non-whitespace token of the comment (the conventional "append"
  # position). This eliminates false positives where the literal token appears
  # mid-description — e.g. `/fix the --no-merge stripping is greedy` must NOT
  # hold and must NOT be mangled. A leading or mid position (`/implement
  # --no-merge add cache`) is treated as ordinary description text; users
  # append the flag at the end (`/implement add cache --no-merge`).
  HOLD_MERGE=0
  if printf '%s' "$comment_body" | grep -Eq '(^|[[:space:]])--no-merge[[:space:]]*$'; then
    # Strip the trailing flag token from NOTE.
    NOTE="$(printf '%s' "$NOTE" | sed -E 's/(^|[[:space:]])--no-merge[[:space:]]*$//; s/^[[:space:]]*//; s/[[:space:]]*$//')"
    case "$COMMAND" in
      /fix|/implement|/test|/security) HOLD_MERGE=1 ;;
    esac
  fi

  # Write context header + core rules
  cat "$CONTEXT_FILE" > "$OUT"
  printf '\n\n---\n\n' >> "$OUT"
  cat "$CORE_FILE" >> "$OUT"
  printf '\n\n---\n\n' >> "$OUT"

  # Route to command-specific prompt
  case "$COMMAND" in
    /review)
      cat "${prompts_dir}/pr-review.md" >> "$OUT"
      if [ -n "$pr_head_sha" ]; then
        printf '\n\n**Commit under review (headRefOid):** `%s`\nRecord this exact SHA in the `**Commit reviewed:**` line at the top of your review body.\n' "$pr_head_sha" >> "$OUT"
      fi
      if [ -n "$NOTE" ]; then
        printf '\n\nAdditional review focus from collaborator:\n\n> %s\n' "$NOTE" >> "$OUT"
      fi
      printf '\n\nA collaborator requested an explicit review using `/review`.\n' >> "$OUT"
      # PR context only: an issue-context /review has no PR verdict to deliver.
      if [ -n "$pr_url" ] || [ "$event_name" = "pull_request_review_comment" ]; then
        printf '%s' "$fallback_directive" >> "$OUT"
      fi
      ;;
    /fix)
      cat "${prompts_dir}/fix.md" >> "$OUT"
      printf '\n\n' >> "$OUT"
      cat "$WORKFLOW_FILE" >> "$OUT"
      if [ -n "$NOTE" ]; then
        printf '\n\nBug or issue to fix:\n\n> %s\n' "$NOTE" >> "$OUT"
      fi
      ;;
    /implement)
      cat "${prompts_dir}/implement.md" >> "$OUT"
      printf '\n\n' >> "$OUT"
      cat "$WORKFLOW_FILE" >> "$OUT"
      if [ -n "$NOTE" ]; then
        printf '\n\nFeature or story to implement:\n\n> %s\n' "$NOTE" >> "$OUT"
      fi
      ;;
    /test)
      cat "${prompts_dir}/test.md" >> "$OUT"
      printf '\n\n' >> "$OUT"
      cat "$WORKFLOW_FILE" >> "$OUT"
      if [ -n "$NOTE" ]; then
        printf '\n\nTarget to write or improve tests for:\n\n> %s\n' "$NOTE" >> "$OUT"
      fi
      ;;
    /security)
      cat "${prompts_dir}/security.md" >> "$OUT"
      printf '\n\n' >> "$OUT"
      cat "$WORKFLOW_FILE" >> "$OUT"
      if [ -n "$NOTE" ]; then
        printf '\n\nSecurity review focus:\n\n> %s\n' "$NOTE" >> "$OUT"
      fi
      ;;
    /analyze)
      cat "${prompts_dir}/analyze.md" >> "$OUT"
      if [ -n "$NOTE" ]; then
        printf '\n\nAnalysis focus:\n\n> %s\n' "$NOTE" >> "$OUT"
      fi
      ;;
    /explain)
      cat "${prompts_dir}/explain.md" >> "$OUT"
      if [ -n "$NOTE" ]; then
        printf '\n\nTopic to explain:\n\n> %s\n' "$NOTE" >> "$OUT"
      fi
      ;;
    /triage)
      cat "${prompts_dir}/triage.md" >> "$OUT"
      if [ -n "$NOTE" ]; then
        printf '\n\nTriage context:\n\n> %s\n' "$NOTE" >> "$OUT"
      fi
      ;;
    /design)
      cat "${prompts_dir}/design.md" >> "$OUT"
      printf '\n\n' >> "$OUT"
      cat "$WORKFLOW_FILE" >> "$OUT"
      if [ -n "$NOTE" ]; then
        printf '\n\nDesign focus / topic:\n\n> %s\n' "$NOTE" >> "$OUT"
      fi
      ;;
    /merge)
      cat "${prompts_dir}/merge.md" >> "$OUT"
      ;;
    /help)
      cat "${prompts_dir}/help.md" >> "$OUT"
      ;;
    /ai)
      if [ -n "$NOTE" ]; then
        printf 'A collaborator has asked:\n\n> %s\n\nAddress this request. For any code changes: create a feature branch, open a PR, never commit to main directly.\n' "$NOTE" >> "$OUT"
      elif [ -n "$pr_url" ] || [ "$event_name" = "pull_request_review_comment" ]; then
        cat "${prompts_dir}/pr-review.md" >> "$OUT"
        if [ -n "$pr_head_sha" ]; then
          printf '\n\n**Commit under review (headRefOid):** `%s`\nRecord this exact SHA in the `**Commit reviewed:**` line at the top of your review body.\n' "$pr_head_sha" >> "$OUT"
        fi
        printf '\n\nA collaborator triggered a re-review by posting `/ai`. Re-assess the PR in full, taking into account all commits pushed since the last review. Note what changed since the previous review if one exists.\n' >> "$OUT"
        printf '%s' "$fallback_directive" >> "$OUT"
      else
        cat "${prompts_dir}/issue-responder.md" >> "$OUT"
        printf '\n\nA collaborator triggered you by posting `/ai`. Analyze the full issue thread and take the appropriate action.\n' >> "$OUT"
      fi
      ;;
  esac

  # If --no-merge was passed on a code-change command, override the
  # Code Change Workflow's auto-merge (step 7). Placed last so it is the
  # final, unambiguous instruction. `/design` holds via its own prompt;
  # `/merge` is the explicit release and is never held.
  if [ "$HOLD_MERGE" = "1" ]; then
    printf '\n\n---\n\n**MERGE HOLD (`--no-merge`):** The collaborator passed `--no-merge` on this `%s`. Override Code Change Workflow step 7: after the automated review posts APPROVE, do NOT merge. Post a comment on the PR stating it is approved and awaiting an explicit `/merge` from a collaborator. Do not merge until `/merge` is received.\n' "$COMMAND" >> "$OUT"
  fi

  export COMMAND NOTE HOLD_MERGE OUT_FILE
}

# When executed directly (not sourced), run the router and print resolved vars.
if [ "${BASH_SOURCE[0]:-$0}" = "$0" ]; then
  route_command
  printf 'COMMAND=%s\nNOTE=%s\nHOLD_MERGE=%s\nOUT_FILE=%s\n' "$COMMAND" "$NOTE" "$HOLD_MERGE" "$OUT_FILE"
fi
