#!/usr/bin/env bash
# classify-lane.sh — review-gate v1 lane classifier (docs/review-gate-v1-spec.md §1).
#
# Reads a `git diff --name-only <merge-base>..HEAD` listing (one path per
# line) from stdin or --diff-file <path> and prints exactly one line:
#
#   lane=docs — every changed path is docs-ish: matches
#               ^(\.github/prompts/|worklogs/|design/|docs/|.*/)$ or ends .md
#               AND no changed file carries an executable extension
#               (.sh .py .go .ts .js .rb .bash) or a shebang (#!) on its
#               first line (checked against the working tree relative to cwd;
#               a deleted/unreadable path cannot carry a shebang and is
#               skipped — a deletion alone never widens the lane).
#   lane=full — anything else.
#
# Why path+content here and not in the prompt: design principle 7 — lane
# classification is computed by script so the reviewer can never talk its way
# into the lenient lane. The shebang/extension clause plugs the escape hatch:
# a "docs-looking" file that is actually executable code (design/helper.sh, an
# extensionless runbook with #!/bin/sh) must force the full lane, or docs-lane
# merges ship live code past review. An empty diff classifies as docs
# (vacuous all()), matching the convergence predicate's empty-ledger case.
#
# Total classification: for ANY input listing the script prints a lane and
# exits 0. Only invocation errors — unknown flag, --diff-file without an
# operand, unreadable --diff-file — exit 2; those are not classifications.
#
# Usage:
#   git diff --name-only "$MERGE_BASE..HEAD" | classify-lane.sh
#   classify-lane.sh --diff-file <path>

set -euo pipefail

diff_file=""
while [ $# -gt 0 ]; do
  case "$1" in
    --diff-file)
      [ $# -ge 2 ] || { printf 'classify-lane.sh: --diff-file requires an operand\n' >&2; exit 2; }
      diff_file="$2"; shift 2 ;;
    --diff-file=*) diff_file="${1#*=}"; shift ;;
    *) printf 'classify-lane.sh: unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

declare -a paths=()
if [ -n "$diff_file" ]; then
  if [ ! -r "$diff_file" ]; then
    printf 'classify-lane.sh: diff file not readable: %s\n' "$diff_file" >&2
    exit 2
  fi
  mapfile -t paths < "$diff_file"
else
  mapfile -t paths
fi

lane=docs
for p in "${paths[@]}"; do
  [ -n "$p" ] || continue
  if ! [[ "$p" =~ ^(\.github/prompts/|worklogs/|design/|docs/|.*/)$ ]] && [[ "$p" != *.md ]]; then
    lane=full
    break
  fi
  case "$p" in
    *.sh|*.py|*.go|*.ts|*.js|*.rb|*.bash) lane=full; break ;;
  esac
  if [ -f "$p" ]; then
    first="$(head -n 1 -- "$p" 2>/dev/null || true)"
    case "$first" in
      '#!'*) lane=full; break ;;
    esac
  fi
done

echo "lane=$lane"
exit 0
