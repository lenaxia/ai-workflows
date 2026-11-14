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
# delivered=false is reserved for "neither channel could post" AND (review
# gate v1) "a verdict was delivered but its body is invalid" — see the
# validation layer below. A stale-sha marker or a stale review NEVER counts,
# and a review-shaped comment WITHOUT the marker does not count either (that
# is the "dumped verdict" class — salvage-verdict.sh owns upgrading it to an
# official review; gates re-run this script fresh afterwards).
#
# Channel-resolution lines on stdout are deliberate: they are the
# production proof that both channels were evaluated AND the gate-parsed
# contract — both verify steps sed-extract the channel from the exact line
# `Verdict delivery channel: <channel>` (pinned by
# TestVerifyGatesParseCheckVerdictContract), so that prefix must never be
# reworded.
#
# Output (GITHUB_OUTPUT when set): delivered=review|comment|false, plus
# rejection_reason=<machine-readable one-liner> when a detected verdict body
# failed review-gate v1 validation (spec docs/review-gate-v1-spec.md §4).
# The rejection reason feeds pr-review.yml's bounded retry, whose prompt
# carries it verbatim ("invalid verdicts feed the retry like undelivered
# verdicts do").
#
# Exit status: 0 whenever a decision was reached (an INVALID verdict is a
# decision — delivered=false + rejection_reason, exit 0); a hard API
# failure exits non-zero (callers mark the step continue-on-error so a
# transient blip degrades to the verify gate's own fresh run, never a
# silent pass).
#
# Usage:
#   REPOSITORY=owner/repo PR_NUMBER=NNN PR_HEAD_SHA=<40-hex> GH_TOKEN=<token> \
#   [GATE_ROUND=n] [GATE_CI_STATUS=success|failure|pending|none] \
#   [GATE_LANE=full|docs] [GATE_BASE_REF=main] [GATE_CHANGED_FILES="a b"] \
#   check-verdict.sh
#
# Review-gate v1 env (all optional; checks degrade to skips when unset —
# callers that never injected the context, e.g. ai-comment.yml's /review
# tail, keep the checks they can run and skip the rest):
#   GATE_ROUND         injected round number; ledger `round:` must match
#   GATE_CI_STATUS     injected CI state; any ci=<state> claim in the body
#                      must match it (spec design principle 6: CI state is
#                      injected into the prompt, polling is pointless, and
#                      script validates CI claims against the injection)
#   GATE_LANE          injected lane; ledger `lane:` must match
#   GATE_BASE_REF      base branch for the changed-file diff (default main)
#   GATE_CHANGED_FILES override for the changed-file list (test/CI-hermeticity
#                      escape hatch; production derives it from git — comma,
#                      space or newline separated; paths containing spaces
#                      are not supported by this override)

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
# preflight, prompt build, and cache restore — MINUS the debounce quiet
# window (DEBOUNCE_SECONDS, emitted by the lever-4 sleep step; the gate
# must measure the pass's own lifetime, never the wait). A structurally-
# doomed first pass therefore needs to die >3min minus that overhead
# before the gate bites; the residual overlap is accepted because the
# gate's premise is a 0% observed rescue rate (fail direction: a
# too-generous retry wastes one bounded pass, never loses a verdict).
# Two known edges: `date -d` is GNU-coreutils (the runner fleet is
# Linux-only; a BSD date would fail open to "no run_minutes"), and a
# clock-skewed FUTURE started_at yields a negative run_minutes, which
# fails CLOSED (retry skipped) — pinned visibly rather than silently.
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
      # Lever 4 coupling: subtract the debounce quiet window so run_minutes
      # keeps measuring the first pass's own lifetime. Without this, a
      # 10-minute default debounce makes every structurally-doomed first
      # pass read >=10 minutes and the >=3 gate never fires (PR #69's gate
      # defeated by lever 4's sleep). DEBOUNCE_SECONDS is unset when the
      # debounce is disabled/skipped — treat that (and any non-numeric
      # value) as 0, fail-open like the rest of this block.
      debounce_seconds=0
      case "${DEBOUNCE_SECONDS:-}" in
        ''|*[!0-9]*) ;;
        *) debounce_seconds="${DEBOUNCE_SECONDS}" ;;
      esac
      run_minutes=$(( (now_epoch - started_epoch - debounce_seconds) / 60 ))
      echo "run_minutes=${run_minutes}" >> "${GITHUB_OUTPUT}"
      echo "First pass has lived ${run_minutes}m (structural-failure gate for the retry; ${debounce_seconds}s of debounce subtracted)."
    fi
  fi
fi

# --- Review-gate v1: delivered-verdict-body validation --------------------------
# Spec docs/review-gate-v1-spec.md §4. Runs ONLY when a verdict was detected
# through either channel; on the first violation the caller emits
# delivered=false + rejection_reason (exit 0 — a decision was reached).
#
#   a. exactly one parseable REVIEW-LEDGER v1 block; `head:` == PR_HEAD_SHA
#      (the head at prompt-build time); `round:` == GATE_ROUND (skipped when
#      unset); `lane:` == GATE_LANE (skipped when unset);
#   b. class map (script-enforced, design principle 2): `verdict: approve`
#      is valid only when the computed open-blocking count is 0. Blocking =
#      {correctness, robustness, security, test-integrity} on lane=full,
#      {docs-truth} on lane=docs. status=fixed closes a finding; `waive:`
#      lines close ADVISORY findings only (a waive naming a blocking-class
#      id is ignored here; reconcile-ledger.sh surfaces it as
#      rejected_waivers at merge time). A class outside the closed
#      vocabulary, or an anchor outside {issue:<n>, rule:<n>, claim:"..."},
#      invalidates the verdict (the reviewer prompt promises exactly this).
#      Docs-lane verdicts may not raise findings on non-docs paths (path
#      mismatch = invalid verdict).
#   c. coverage: lane=full → every changed file appears on a `coverage:`
#      line; lane=docs → every changed file is a docs-lane path. Changed
#      files: GATE_CHANGED_FILES override, else
#      `git diff --name-only origin/<GATE_BASE_REF|main>...HEAD` (the
#      consumer checkout is fetch-depth: 0, so the merge-base diff is
#      available locally — no network). If the diff is unobtainable the
#      coverage check is SKIPPED fail-open with a log line: a tooling gap
#      must never reject an honest verdict.
#   d. pointer spot-check: up to 3 sampled finding pointers (blocking-class
#      first — higher signal) must exist in the checkout with the line
#      within file length. Worktree-based approximation of "exists at the
#      reviewed head": the consumer checkout is at the reviewed content for
#      review purposes (actions/checkout materializes refs/pull/N/merge;
#      unmergeable PRs are unreviewable anyway). Documented in README
#      "Review gate v1".
#   e. any ci=<state> claim in the body must match GATE_CI_STATUS (skipped
#      when unset).
#
# Why validation lives HERE and not in reconcile-ledger.sh: this script is
# the file-ownership boundary of a different workstream — the class map is
# duplicated deliberately, self-contained, so check-verdict.sh stays
# runnable in tests with only bash/git/jq and a mock gh (PyYAML not
# assumed anywhere per the spec's runtime floor).
GV_BLOCKING_FULL=" correctness robustness security test-integrity "

# gate_trim strips leading/trailing whitespace (bash-only, no xargs quoting
# hazards on adversarial prose).
gate_trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "${s}"
}

# gate_is_blocking <class> <lane> — exit 0 iff the class is merge-gating in
# the lane (the script-owned class→blocking map; the LLM never writes
# "blocking").
gate_is_blocking() {
  if [ "$2" = "docs" ]; then
    [ "$1" = "docs-truth" ]
    return
  fi
  case "${GV_BLOCKING_FULL}" in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

# gate_docs_path <path> — exit 0 iff the path is docs-lane-allowed (the
# classify-lane.sh path set restricted to files: docs/ prefixes or .md).
gate_docs_path() {
  printf '%s' "$1" | grep -Eq '^(\.github/prompts/|worklogs/|design/|docs/)|\.md$'
}

# gate_validate <body> — prints the rejection reason ("" = valid). ALWAYS
# exits 0 (the caller runs under set -e inside a command substitution).
gate_validate() {
  local body="$1"
  local tmp blockfile
  tmp="$(mktemp "${TMPDIR:-/tmp}/checkverdict.XXXXXX")"
  blockfile="${tmp}.block"
  local reason=""
  local lane="" verdict="" lround="" lhead=""
  local -a cov_lines=() findings=() waived=()
  local demoted_ids=""   # |id|… of findings whose anchor fell outside the closed list
  printf '%s' "${body}" > "${tmp}"

  # --- a1: exactly one ledger block -------------------------------------------
  local nblocks
  nblocks="$(grep -c '^REVIEW-LEDGER v1[[:space:]]*$' "${tmp}" || true)"
  if [ "${nblocks}" -eq 0 ]; then
    reason="missing-ledger-block"
  elif [ "${nblocks}" -gt 1 ]; then
    reason="duplicate-ledger-block:${nblocks}"
  fi

  # --- a2: parse the block ------------------------------------------------------
  if [ -z "${reason}" ]; then
    sed -n '/^REVIEW-LEDGER v1[[:space:]]*$/,/^END-REVIEW-LEDGER[[:space:]]*$/p' "${tmp}" > "${blockfile}"
    local line val part fid fclass fptr fanchor fstatus fpath fline wid
    local -a parts
    while IFS= read -r line; do
      line="$(gate_trim "${line}")"
      case "${line}" in
        "REVIEW-LEDGER v1"|"END-REVIEW-LEDGER"|"") continue ;;
        round:*)
          val="$(gate_trim "${line#round:}")"
          if [[ "${val}" =~ ^[0-9]+$ ]]; then lround="${val}"; else reason="ledger-malformed:round"; break; fi
          ;;
        head:*)
          val="$(gate_trim "${line#head:}")"
          if [[ "${val}" =~ ^[0-9a-f]{40}$ ]]; then lhead="${val}"; else reason="ledger-malformed:head"; break; fi
          ;;
        lane:*)
          val="$(gate_trim "${line#lane:}")"
          case "${val}" in
            full|docs) lane="${val}" ;;
            *) reason="ledger-malformed:lane"; break ;;
          esac
          ;;
        verdict:*)
          val="$(gate_trim "${line#verdict:}")"
          case "${val}" in
            approve|request-changes) verdict="${val}" ;;
            *) reason="ledger-malformed:verdict"; break ;;
          esac
          ;;
        coverage:*) cov_lines+=("$(gate_trim "${line#coverage:}")") ;;
        finding:*)
          fid=""; fclass=""; fptr=""; fanchor=""; fstatus=""
          IFS=';' read -ra parts <<< "${line#finding:}"
          for part in "${parts[@]}"; do
            part="$(gate_trim "${part}")"
            case "${part}" in
              id=*) fid="${part#id=}" ;;
              class=*) fclass="${part#class=}" ;;
              anchor=*) fanchor="${part#anchor=}" ;;
              pointer=*) fptr="${part#pointer=}" ;;
              status=*) fstatus="${part#status=}" ;;
              risk=*|fix_cost=*|"") : ;;   # prose fields — dropped after parse
              *) : ;;                       # unknown keys tolerated (forward-compat)
            esac
          done
          if [ -z "${fid}" ] || [ -z "${fclass}" ] || [ -z "${fptr}" ] || [ -z "${fstatus}" ]; then
            reason="ledger-malformed:finding-line"; break
          fi
          case "${fclass}" in
            correctness|robustness|security|test-integrity|docs-truth|style) ;;
            *) reason="unknown-class:${fclass}"; break ;;
          esac
          case "${fstatus}" in
            new|fixed|still-open) ;;
            *) reason="ledger-malformed:finding-line"; break ;;
          esac
          # Anchor normalization + demotion (spec §3: "Unresolvable anchor →
          # finding auto-demoted to advisory"). Two live lessons from the
          # v0.2.28 rollout (PR #2034, three red rounds):
          #   1. The strict `claim:".*"` QUOTE requirement rejected reviewers
          #      writing the natural unquoted `claim:text` — annotation
          #      cosmetics invalidating an otherwise-complete verdict.
          #      Normalized: quotes stripped, both shapes accepted.
          #   2. Any anchor outside the closed list DEMOTES the finding to
          #      advisory instead of killing the verdict — a demoted finding
          #      is visible in the ledger for audit (the severity-laundering
          #      counterweight), while the verdict's other findings and the
          #      class map keep working.
          fanchor="${fanchor//\"/}"
          # [[ =~ ]] — bash REGEX, not case-glob: `issue:[0-9]+` inside a
          # case pattern means "one digit then a literal +", which silently
          # demoted even perfectly-formed `issue:3` anchors.
          if [[ "${fanchor}" =~ ^(issue:[0-9]+|rule:[0-9]+|claim:.+)$ ]]; then
            :
          else
            demoted_ids="${demoted_ids}|${fid}|"
            echo "gate: finding ${fid} anchor '${fanchor}' is outside the closed list — demoted to advisory (spec §3)" >&2
          fi
          fpath="${fptr%:*}"
          fline="${fptr##*:}"
          if [ -z "${fpath}" ] || ! [[ "${fline}" =~ ^[0-9]+$ ]]; then
            reason="ledger-malformed:finding-line"; break
          fi
          findings+=("${fid}|${fclass}|${fpath}|${fline}|${fstatus}")
          ;;
        waive:*)
          # `waive: id=<slug>; reason=<one line>` — the author's waiver
          # comment is the authoritative record (Waivers section of the
          # spec); this body line MIRRORS it. Advisory-only: a waive
          # naming a blocking-class id closes nothing (and such a finding
          # keeps an approve invalid via the class map below);
          # reconcile-ledger.sh reports it as rejected_waivers.
          wid=""
          IFS=';' read -ra parts <<< "${line#waive:}"
          for part in "${parts[@]}"; do
            part="$(gate_trim "${part}")"
            case "${part}" in
              id=*) wid="${part#id=}" ;;
              reason=*) : ;;
              "") : ;;
            esac
          done
          [ -n "${wid}" ] && waived+=("${wid}")
          ;;
        *) : ;;   # unknown block lines tolerated (forward-compat)
      esac
    done < "${blockfile}"
    if [ -z "${reason}" ]; then
      if [ -z "${lround}" ]; then reason="ledger-malformed:round"
      elif [ -z "${lhead}" ]; then reason="ledger-malformed:head"
      elif [ -z "${lane}" ]; then reason="ledger-malformed:lane"
      elif [ -z "${verdict}" ]; then reason="ledger-malformed:verdict"
      fi
    fi
  fi

  # --- a3: head / injected round / injected lane -------------------------------
  if [ -z "${reason}" ] && [ "${lhead}" != "${PR_HEAD_SHA}" ]; then
    reason="head-mismatch"
  fi
  if [ -z "${reason}" ] && [ -n "${GATE_ROUND:-}" ]; then
    if [ "${lround}" != "${GATE_ROUND}" ]; then reason="round-mismatch"; fi
  fi
  if [ -z "${reason}" ] && [ -n "${GATE_LANE:-}" ]; then
    # Lane normalization, not rejection: the injected lane is authoritative
    # (classify-lane.sh is path-based; the reviewer merely restates it). A
    # mislabeled `lane:` used to kill the whole verdict (lane-mismatch, PR
    # #2034 r1) — now it is corrected and the class map below applies with
    # the gate's lane.
    if [ "${lane}" != "${GATE_LANE}" ]; then
      echo "gate: verdict lane '${lane}' corrected to injected lane '${GATE_LANE}' (the gate's lane is authoritative)" >&2
      lane="${GATE_LANE}"
    fi
  fi

  # --- b: script-computed class map + docs-lane finding scope ------------------
  if [ -z "${reason}" ]; then
    local f fid fclass fpath fline fstatus open_blocking=0
    for f in ${findings[@]:+"${findings[@]}"}; do
      IFS='|' read -r fid fclass fpath fline fstatus <<< "${f}"
      # Demoted findings (anchor outside the closed list) never count as
      # merge-gating — see the anchor normalization note above.
      if [[ "${demoted_ids}" == *"|${fid}|"* ]]; then
        :
      elif gate_is_blocking "${fclass}" "${lane}" && [ "${fstatus}" != "fixed" ]; then
        open_blocking=$((open_blocking + 1))
      fi
      if [ "${lane}" = "docs" ] && ! gate_docs_path "${fpath}"; then
        reason="docs-lane-finding:${fpath}"
        break
      fi
    done
    if [ -z "${reason}" ] && [ "${verdict}" = "approve" ] && [ "${open_blocking}" -gt 0 ]; then
      # The reviewer does not compute this count — it emits classes and the
      # script counts (design principle 2). An approve that disagrees with
      # the computed count is dishonest-by-accident at best.
      reason="approve-with-open-blocking:${open_blocking}"
    fi
  fi
  if [ -z "${reason}" ] && [ -n "${waived[*]:-}" ]; then
    # stderr, not stdout: gate_validate's stdout IS the reason (the caller
    # captures it via command substitution) — a log line on stdout would
    # pollute the rejection_reason and false-reject valid verdicts.
    echo "review-gate: ${#waived[@]} advisory waive line(s) mirrored in the verdict body (author comments remain the authoritative record)." >&2
  fi

  # --- c: coverage vs changed files --------------------------------------------
  if [ -z "${reason}" ]; then
    local changed="" cf cov_flat missing=""
    if [ -n "${GATE_CHANGED_FILES:-}" ]; then
      changed="$(printf '%s' "${GATE_CHANGED_FILES}" | tr ', \t' '\n\n\n' | sed '/^$/d' || true)"
    else
      # Three-dot diff = merge-base(origin/<base>, HEAD)..HEAD — exactly the
      # PR's changed-file list; the consumer checkout is fetch-depth: 0 so
      # this is local. Unobtainable diff ⇒ check skipped fail-open (logged).
      changed="$(git diff --name-only "origin/${GATE_BASE_REF:-main}...HEAD" 2>/dev/null | tr -d '\r' || true)"
    fi
    if [ -n "${changed}" ]; then
      if [ "${lane}" = "docs" ]; then
        while IFS= read -r cf; do
          [ -n "${cf}" ] || continue
          if ! gate_docs_path "${cf}"; then reason="docs-lane-scope:${cf}"; break; fi
        done <<< "${changed}"
      else
        cov_flat="${cov_lines[*]:-}"
        cov_flat="${cov_flat//,/ }"
        while IFS= read -r cf; do
          [ -n "${cf}" ] || continue
          # Exact-path membership (paths with spaces unsupported — the
          # reviewer prompt's coverage contract lists changed paths verbatim
          # from the injected list).
          if ! printf '%s\n' ${cov_flat} | grep -qxF -- "${cf}"; then
            missing="${missing:+${missing},}${cf}"
          fi
        done <<< "${changed}"
        if [ -n "${missing}" ]; then
          reason="coverage-missing:$(printf '%s' "${missing}" | cut -d, -f1-3)"
        fi
      fi
    else
      # stderr, not stdout — same command-substitution contract as above.
      echo "review-gate: changed-file list unavailable (no override; git diff failed or empty) — coverage check skipped (fail-open)." >&2
    fi
  fi

  # --- d: pointer spot-check (≤3 samples, blocking first) ----------------------
  if [ -z "${reason}" ]; then
    local f fid fclass fpath fline fstatus checked=0 nlines
    local -a order=()
    for f in ${findings[@]:+"${findings[@]}"}; do
      IFS='|' read -r fid fclass fpath fline fstatus <<< "${f}"
      if gate_is_blocking "${fclass}" "${lane}"; then order+=("${f}"); fi
    done
    for f in ${findings[@]:+"${findings[@]}"}; do
      IFS='|' read -r fid fclass fpath fline fstatus <<< "${f}"
      if ! gate_is_blocking "${fclass}" "${lane}"; then order+=("${f}"); fi
    done
    for f in ${order[@]:+"${order[@]}"}; do
      [ "${checked}" -lt 3 ] || break
      IFS='|' read -r fid fclass fpath fline fstatus <<< "${f}"
      checked=$((checked + 1))
      if [ ! -f "${fpath}" ]; then
        reason="invalid-pointer:${fpath}:${fline}"
        break
      fi
      # awk NR (not wc -l): counts a final line without trailing newline.
      nlines="$(awk 'END {print NR}' "${fpath}" 2>/dev/null || echo 0)"
      if [ "${fline}" -lt 1 ] || [ "${fline}" -gt "${nlines}" ]; then
        reason="invalid-pointer:${fpath}:${fline}"
        break
      fi
    done
  fi

  # --- e: CI claims vs injected GATE_CI_STATUS ---------------------------------
  if [ -z "${reason}" ] && [ -n "${GATE_CI_STATUS:-}" ]; then
    local claim st
    while IFS= read -r claim; do
      [ -n "${claim}" ] || continue
      st="$(printf '%s' "${claim}" | grep -Eio '(success|failure|pending|none)' | head -1 || true)"
      st="${st,,}"
      if [ -n "${st}" ] && [ "${st}" != "${GATE_CI_STATUS,,}" ]; then
        reason="ci-claim-mismatch:${st}"
        break
      fi
    done < <(grep -Eio '\bci([-_ ]?status)?[=:][[:space:]]*(success|failure|pending|none)\b' "${tmp}" | sort -u || true)
  fi

  rm -f "${tmp}" "${blockfile}"
  printf '%s' "${reason}"
}

# --- Channel 1: official review ------------------------------------------------
# --paginate + per-element emission: /pulls/N/reviews pages at 30, oldest
# first, so a busy PR's head-SHA verdict can sit beyond page 1. Each
# matching review's body is emitted base64-encoded (one line per review —
# bodies are multiline, base64 is the only line-safe transport through the
# --paginate pipe); the LAST match wins (GitHub returns reviews oldest
# first, so the last line is the operative/newest verdict for this head).
review_out="$(gh api "repos/${REPOSITORY}/pulls/${PR_NUMBER}/reviews" --paginate \
  --jq '.[] | select(.user.login == "github-actions[bot]") | select((.state == "APPROVED" or .state == "CHANGES_REQUESTED") and .commit_id == "'"${PR_HEAD_SHA}"'") | ((.body // "") | @base64)' \
  | awk '{last=$1} END {print NR "\t" last}')"
review_count="${review_out%%$'\t'*}"
review_body=""
if [ "${review_count}" -gt 0 ]; then
  review_body="$(printf '%s' "${review_out#*$'\t'}" | base64 -d)"
fi

if [ "${review_count}" -gt 0 ]; then
  echo "Channel review: ${review_count} official bot verdict(s) against HEAD."
  rejection="$(gate_validate "${review_body}")"
  if [ -n "${rejection}" ]; then
    echo "Verdict rejected by review-gate validation: ${rejection} (delivered=false; the bounded retry prompt receives this reason)."
    emit false
    [ -n "${GITHUB_OUTPUT:-}" ] && echo "rejection_reason=${rejection}" >> "${GITHUB_OUTPUT}"
    exit 0
  fi
  emit review
  exit 0
fi
echo "Channel review: no official bot verdict against HEAD — checking comment fallback channel."

# --- Channel 2: marker verdict comment ------------------------------------------
# Newest-agnostic existence check across ALL pages (--paginate --slurp;
# gh rejects --slurp with --jq, so selection runs in an external jq pipe —
# same contract as salvage-verdict.sh). The marker must carry the FULL head
# SHA; a short or differing sha is a stale verdict and never counts. The
# newest matching marker's body rides along (base64) for the §4 validation —
# last match in list order = newest comment.
marker_out="$(gh api "repos/${REPOSITORY}/issues/${PR_NUMBER}/comments" --paginate --slurp \
  | jq -r --arg sha "${PR_HEAD_SHA}" '
    [.[] | .[]
      | select(.user.login == "github-actions[bot]")
      | select((.body // "") | test("<!-- ai-workflows-verdict: (APPROVE|REQUEST_CHANGES) sha=[0-9a-f]{40} -->"))
      | (.body | capture("ai-workflows-verdict: (?:APPROVE|REQUEST_CHANGES) sha=(?<sha>[0-9a-f]{40})")) as $c
      | {sha: $c.sha, b64: (.body | @base64)}
      | select(.sha == $sha)]
    | if length > 0 then (length | tostring) + "\n" + (last | .b64 | @base64d) else "0" end')"
marker_count="${marker_out%%$'\n'*}"
marker_body=""
if [ "${marker_count}" -gt 0 ]; then
  marker_body="${marker_out#*$'\n'}"
fi

if [ "${marker_count}" -gt 0 ]; then
  echo "Channel comment: ${marker_count} verdict-marker comment(s) against HEAD (official review channel refused or unavailable)."
  rejection="$(gate_validate "${marker_body}")"
  if [ -n "${rejection}" ]; then
    echo "Verdict rejected by review-gate validation: ${rejection} (delivered=false; the bounded retry prompt receives this reason)."
    emit false
    [ -n "${GITHUB_OUTPUT:-}" ] && echo "rejection_reason=${rejection}" >> "${GITHUB_OUTPUT}"
    exit 0
  fi
  emit comment
  exit 0
fi

echo "Channel comment: no sha-matching verdict marker found."
emit false
