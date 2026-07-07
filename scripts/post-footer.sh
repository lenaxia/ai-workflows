#!/usr/bin/env bash
# post-footer.sh — post the AI command reference comment once per issue/PR
# thread, deduplicated by a hidden HTML marker.
#
# Hoisted out of the three reusable workflows (ai-comment, issue-opened,
# pr-review) so the dedup logic exists in exactly one place. Previously each
# workflow inlined this and the dedup grep drifted between repos:
#   - some used `grep -qE "^${MARKER}"` (treats marker as regex, anchors LoS)
#   - some used `grep -qF "$MARKER"`     (literal match, no anchor)
# This script normalizes on `grep -qF`: the marker is an HTML comment unlikely
# to occur in prose, literal matching avoids regex surprises if the marker
# ever gains metacharacters, and LoS anchoring is unnecessary because the
# marker is posted at the start of its own comment body.
#
# Inputs (environment):
#   GH_TOKEN        — GitHub auth token (secrets.GITHUB_TOKEN)
#   REPOSITORY      — full owner/repo (github.repository)
#   ISSUE_NUMBER    — issue or PR number (github.event.issue.number)
#   PROMPTS_DIR     — directory containing commands-footer.md (.ai-workflows/templates/prompts)
#   MARKER          — dedup marker (default: <!-- ai-commands-footer -->)
#
# Exits 0 in all idempotent-skip cases so the workflow step never fails the
# run just because the footer was skipped.
set -euo pipefail

gh_token="${GH_TOKEN:-}"
repository="${REPOSITORY:-}"
issue_number="${ISSUE_NUMBER:-}"
prompts_dir="${PROMPTS_DIR:-.ai-workflows/templates/prompts}"
marker="${MARKER:-<!-- ai-commands-footer -->}"

if [ -z "$gh_token" ]; then
  echo "GH_TOKEN missing; cannot post footer." >&2
  exit 0
fi
if [ -z "$repository" ]; then
  echo "REPOSITORY missing; cannot post footer." >&2
  exit 0
fi
if [ -z "$issue_number" ]; then
  echo "No issue/PR number resolved; skipping command-reference comment."
  exit 0
fi

footer_src="$prompts_dir/commands-footer.md"
if [ ! -f "$footer_src" ]; then
  echo "commands-footer.md missing at $footer_src; skipping."
  exit 0
fi

# Dedup: scan existing comments for the marker. -F = literal (no regex),
# which is safe regardless of what characters the marker contains.
if gh api "repos/${repository}/issues/${issue_number}/comments" \
     --paginate -q '.[].body' 2>/dev/null | grep -qF -- "$marker"; then
  echo "Command reference already present in thread #${issue_number}; skipping."
  exit 0
fi

# Compose: marker line first (so future dedup scans find it), then the
# footer body.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
{
  printf '%s\n\n' "$marker"
  cat "$footer_src"
} > "$tmp"

GH_TOKEN="$gh_token" gh issue comment "$issue_number" \
  --repo "$repository" --body-file "$tmp"
echo "Posted AI command reference to thread #${issue_number}."
