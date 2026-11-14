# Review Gate v1 — Interface Spec

Single source of truth for the review-loop redesign. Three workstreams implement
against this contract; none may deviate without updating this file.

## Problem being fixed (measured)

- Review loop rounds never converge: 39/20/19/8 blocking rounds observed on
  four PRs; ~91 blocking verdicts across 5 PRs; ~170 compute-hours.
- Per reviewer turn (28.4 min measured): 54% idle-waiting on CI (`gh run watch`
  + sleep-poll loops), 23% generating oversized verdict bodies, 17% actual work.
- Root causes: "any finding, no matter how minor, blocks" + mandatory
  finding-enumeration + full re-review on every push + no round budget +
  reviewer self-assigned CI verification duty.

## Design principles

1. Convergence is defined over a **finding ledger**, never over time/rounds.
2. The LLM chooses **class**; the script maps class→blocking. The LLM never
   writes "blocking".
3. Blocking findings require **anchors** resolvable by script (closed list).
4. Blocking-class findings are **never author-waivable**. Advisory classes are.
5. Round budget caps **demand-generation**, never **fix-verification**.
6. CI state is **injected into the prompt**; polling by the reviewer is
   pointless and forbidden. Script validates CI claims against injection.
7. Lane classification is **path/content-based**, computed by script.
8. Coverage is a **contract**: verdict lists files reviewed; script checks
   against the changed-file list and spot-checks pointers.
9. Merge requires **ledger convergence** + head-match. Stats are posted.
10. Guarantees live in **scripts**; prompts carry only judgment guidance.

## Verdict ledger block format (line-based, v1)

Every formal review verdict body (official review OR verdict-marker comment)
MUST contain exactly one block:

```
REVIEW-LEDGER v1
round: <int>
head: <40-char sha>
lane: full|docs
verdict: approve|request-changes
coverage: <comma/space-separated changed paths actually reviewed>   (repeatable)
finding: id=<slug>; class=<class>; anchor=<anchor>; pointer=<path>:<line>; status=new|fixed|still-open; risk=<one line>; fix_cost=<one line>
waive: id=<slug>; reason=<one line>
END-REVIEW-LEDGER
```

- `class` ∈ `correctness | robustness | security | test-integrity | docs-truth | style`
- `anchor` ∈ `issue:<n>` (criterion n from the linked issue) | `rule:<n>`
  (numbered rule in the consumer's README-LLM.md) | `claim:"<verbatim PR-body substring>"`
- One `finding:` per line. `risk`/`fix_cost` are single-line prose.
- `waive:` lines close advisory findings; the author's waiver comments are the
  authoritative record (see Waivers).
- Advisory findings carried open across rounds keep their ids (stable id =
  stable slug of pointer+class).

### Class→blocking map (script-enforced)

- full lane: blocking = {correctness, robustness, security, test-integrity};
  advisory = {docs-truth, style}
- docs lane: blocking = {docs-truth}; advisory = {style}; docs-lane verdicts
  may not raise findings about non-docs files (path mismatch = invalid verdict)

`verdict: approve` is only valid when the script-computed open-blocking count
is 0. The reviewer does not compute this; it emits classes, the script counts.

## Waivers

Author (or any admin) posts a PR comment containing a line:

```
waive <finding-id>: <reason>
```

Script parses waiver comments, matches by id, only accepts advisory-class ids.
A blocking-class waiver attempt is ignored and surfaced in the reconcile output
as `rejected_waivers`.

## Script CLI contracts

All scripts live in `scripts/review-gate/`. They must run on ubuntu-latest
(bash, git, gh, jq, python3 available; PyYAML NOT assumed). Every script
accepts `--from-file <path>` (or env) to read canned API output in tests, so
golden tests need no network.

1. `classify-lane.sh` — stdin or `--diff-file <path>`: `git diff --name-only
   <merge-base>..HEAD` output. Prints `lane=full` or `lane=docs`. docs iff
   every changed path matches `^(\.github/prompts/|worklogs/|design/|docs/|.*/)$`
   or ends `.md`, AND no added file has a shebang on its first line or an
   executable extension (`.sh|.py|.go|.ts|.js|.rb|.bash`). Else full.

2. `review-state.sh <subcommand>`:
   - `rounds <pr> [--repo R]` → integer: count of prior bot verdicts (formal
     reviews by the bot actor with a `**Commit reviewed:**` line + sha-pinned
     verdict-marker comments)
   - `ci-status <sha> [--repo R]` → `ci=success|failure|pending|none`
     (all runs associated with the sha; any failure → failure; none → none)
   - `head-check <sha> [--repo R]` → exit 0 iff current PR head == sha

3. `reconcile-ledger.sh <pr> [--repo R]` — reads all bot verdict bodies
   (reviews + marker comments), author waiver comments, rebuilds the ledger,
   prints JSON:
   `{round, head, lane, open_blocking:[{id,class,anchor,pointer,risk}],
     open_advisory:[...], waived:[{id,reason}], rejected_waivers:[...],
     converged:<bool>}`
   `converged` = no open blocking AND every open advisory is fixed-or-waived.
   Latest verdict's `verdict:` field is echoed as `last_verdict`.

4. `check-verdict.sh` (extends the existing script) — after the existing
   delivered/review detection, validate the delivered verdict body:
   a. ledger block present and parseable; `head:` matches the PR head at
      prompt-build time (passed in env); `round:` matches injected round.
   b. class map: computed open-blocking == 0 if verdict: approve.
   c. coverage: full lane → every changed file appears in coverage lines;
      docs lane → every changed file is .md/allowed path.
   d. pointer spot-check: sample up to 3 finding pointers; file must exist at
      the reviewed head and line must be within file length.
   e. any CI status claimed in the body must match injected `GATE_CI_STATUS`.
   On violation: emit `delivered=false` with `rejection_reason` (feeds the
   existing bounded-retry path — the retry prompt must include the reason).

## Workflow wiring (`.github/workflows/pr-review.yml`)

- `debounce_minutes` default 10 → 3 (input stays caller-overridable).
- Build-prompt step additionally injects: lane, round number, `ci-status`,
  current ledger JSON, issue acceptance-criteria list (best-effort grep of the
  linked issue), README-LLM.md numbered rule titles (best-effort), PR body,
  and the budget rule text ("after round 3 you may not introduce new blocking
  demand classes; fix-verification is always in scope").
- Verify step: job red iff no verdict delivered OR verdict invalid
  (rejection_reason). Review jobs do NOT fail merely because findings remain
  open — convergence gates merging, not reviewing.

## Merge gate (`templates/prompts/merge.md`)

`/merge` must: run `reconcile-ledger.sh`, refuse unless `converged=true`,
run `head-check <verdict head>`, refuse on mismatch, then squash-merge and
post a stats comment: rounds, findings by class, waived list w/ reasons,
rejected_waivers if any.

## Code-change commands (`templates/prompts/code-change-workflow.md`, `fix.md`,
`implement.md`, `test.md`, `security.md` if they inline the loop)

Closure contract: each round must resolve EVERY open finding (fix) or post
`waive <id>: <reason>` for advisory findings (blocking-class cannot be
waived — escalate instead). No auto-merge unless converged.

## Reviewer prompt contract (`templates/prompts/pr-review.md`)

- Classify; never write "blocking" (the script decides).
- Emit the ledger block. No CI polling commands (`gh run watch`, `sleep`
  loops) — CI state is injected; verdict CI claims are validated.
- Anti-escalation: after round 3, new findings must be shipped-code classes;
  demand-invention beyond that is a process violation.
- Coverage contract: list what you actually read.
- Waiver honoring: findings with a matching author waiver are closed; do not
  re-raise them.
- Evidence: ≤2 consecutive green runs, only lanes reachable from the diff;
  targeted spec-level instruments, never full-suite babysitting.
- Verdict body trimmed: findings + coverage + ledger block + one-paragraph
  summary. Mandatory 10-section essays are deleted; evidence detail belongs in
  the worklog.
