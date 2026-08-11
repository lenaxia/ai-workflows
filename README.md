# lenaxia/ai-workflows

Central source of truth for AI-command workflows across `lenaxia/*` repos.

## What this repo does

Provides reusable GitHub Action workflows, prompt templates, and the
`route-command.sh` router that powers slash-command AI interactions
(`/fix`, `/implement`, `/review`, etc.) on issues and PRs.

Consumer repos (gokore, LLMSafeSpaces, rathena-client, TinyRSVP, containers) reference
this repo via pinned reusable workflows. gokore additionally renders shared
prompt templates; the other consumers fork their prompts (project-specific)
and use this repo for workflow plumbing only (router, footer, reusable
workflows, the formal-blocking-review directive). Prompt files are rendered
from templates by `ai-sync` and committed to each consumer's `.github/prompts/`.

## Architecture

```
ai-workflows/
├── .github/workflows/        # Reusable workflows + CI + propagation
│   ├── ai-comment.yml        #   Called by consumers for /ai, /fix, etc.
│   ├── issue-opened.yml      #   Called when issues are opened
│   ├── pr-review.yml         #   Called on PR open/synchronize
│   ├── propagate.yml         #   On tag push: render + open PRs to consumers
│   └── test-router.yml       #   Runs route-command.sh tests on every push
├── scripts/
│   ├── route-command.sh      # Slash-command router (single source of truth)
│   ├── post-footer.sh         # Once-per-thread footer dedup
│   ├── pre-commit             # Consumer-side hook (rejects edits to managed files)
│   └── ai-sync/               # Renderer: templates + consumer config → files
├── templates/prompts/        # Prompt templates (Go text/template)
├── consumers/                # Per-repo configs + override blocks
│   ├── <repo>.yaml           #   Contract: vars, forked, blocks
│   └── <repo>/               #   Per-repo block overrides
├── tests/                    # Regression tests
│   ├── gharouter/            #   route-command.sh tests (94 subtests)
│   └── aisync/               #   ai-sync renderer tests (7 tests)
├── opencode.json             # Canonical opencode config
└── go.mod
```

## Concurrency / cancellation

Review-producing runs collapse to a single per-PR concurrency group so the
latest review wins; code-change commands are never cancelled mid-run.

| Run | Concurrency group | `cancel-in-progress` |
|---|---|---|
| `pr-review.yml` (auto, open/synchronize) | `ai-review-<PR#>` | true |
| `ai-comment.yml` for `/review` or `/ai` on a PR | `ai-review-<PR#>` (shared with the above) | true |
| `ai-comment.yml` for any other command (`/fix`, `/implement`, `/test`, `/security`, `/design`, `/merge`, …) | `ai-cmd-<PR#>-<comment-id>` (unique per comment) | true (never collides → never cancels) |

So: a new push or a fresh `/review` cancels an in-flight review of a now-stale
commit (which would otherwise waste cost and post conflicting review events on
different SHAs). But a `/review` cannot kill an in-flight `/implement` — they
live in different groups — so code changes always run to completion.

Reviews also state the exact commit they cover: the PR `headRefOid` is injected
into the review prompt, and the review body format requires a leading
`**Commit reviewed:** <SHA>` line.

## File classification contract

Every file in a consumer's `.github/prompts/` falls into one of three buckets:

| Bucket | How it's managed | How to edit |
|---|---|---|
| **Rendered** | Generated from `templates/prompts/` by `ai-sync` | Edit the template here, sync to consumers |
| **Forked** | Listed in `forked:` in consumer config; sync skips it | Edit directly in the consumer repo |
| **Consumer-owned** | Never had a template (e.g. `context.md`) | Edit directly in the consumer repo |

Rendered files carry a managed-file banner. A pre-commit hook in consumers
rejects direct edits to these files — use `forked:` to opt out, or edit the
template here.

## Adding a consumer

### 1. Create a consumer config

Create `consumers/<name>.yaml`. If your project's prompts differ from the
gokore-derived defaults (they almost certainly do), **fork all prompts** so
sync never overwrites them:

```yaml
name: myrepo
version: v0.2.0

vars:
  project_name: MyRepo
  rules_doc: README-LLM.md

forked:
  - context.md
  - core-rules.md
  - analyze.md
  - code-change-workflow.md
  - commands-footer.md
  - design.md
  - explain.md
  - fix.md
  - help.md
  - implement.md
  - issue-responder.md
  - merge.md
  - pr-review.md
  - security.md
  - test.md
  - triage.md

blocks: []
```

See [Templates are goKore-derived](#templates-are-gokore-derived) for why.

### 2. Add the consumer to propagate.yml

Add `<name>` to the matrix in `.github/workflows/propagate.yml`.

### 3. Create the caller workflows in the consumer repo

Add three files to the consumer's `.github/workflows/`. Each is a thin caller
that delegates to the reusable workflow in this repo. **The `uses:` ref must
be a hardcoded tag** — see [Lessons learned](#lessons-learned) for why.

```yaml
# .github/workflows/issue-opened.yml
name: Issue Opened

on:
  issues:
    types: [opened]

permissions:
  id-token: write
  contents: read
  issues: write
  pull-requests: write

jobs:
  respond:
    uses: lenaxia/ai-workflows/.github/workflows/issue-opened.yml@v0.2.0
    secrets: inherit
    with:
      version: v0.2.0
      project_name: myrepo
```

```yaml
# .github/workflows/pr-review.yml
name: PR Review

on:
  pull_request:
    types: [opened, synchronize]

permissions:
  id-token: write
  contents: read
  issues: write
  pull-requests: write

jobs:
  review:
    # No `if:` filter needed for the common automation bots — the reusable
    # workflow at @v0.2.9+ skips renovate[bot], github-actions[bot],
    # release-please[bot], dependabot[bot], mergify[bot], and
    # github-merge-queue[bot] at the job level. Add your own `if:` only
    # if you want to filter additional actors.
    uses: lenaxia/ai-workflows/.github/workflows/pr-review.yml@v0.2.9
    secrets: inherit
    with:
      version: v0.2.9
      project_name: myrepo
```

```yaml
# .github/workflows/ai-comment.yml
name: AI Commands

on:
  issue_comment:
    types: [created]
  pull_request_review_comment:
    types: [created]

permissions:
  id-token: write
  contents: write          # write — this workflow allows code changes
  issues: write
  pull-requests: write

jobs:
  respond:
    if: |
      (startsWith(github.event.comment.body, '/ai') ||
       startsWith(github.event.comment.body, '/review') ||
       startsWith(github.event.comment.body, '/fix') ||
       startsWith(github.event.comment.body, '/implement') ||
       startsWith(github.event.comment.body, '/analyze') ||
       startsWith(github.event.comment.body, '/test') ||
       startsWith(github.event.comment.body, '/security') ||
       startsWith(github.event.comment.body, '/explain') ||
       startsWith(github.event.comment.body, '/triage') ||
       startsWith(github.event.comment.body, '/help') ||
       startsWith(github.event.comment.body, '/design') ||
       startsWith(github.event.comment.body, '/merge') ||
       contains(github.event.comment.body, ' /ai') ||
       contains(github.event.comment.body, ' /review') ||
       contains(github.event.comment.body, ' /fix') ||
       contains(github.event.comment.body, ' /implement') ||
       contains(github.event.comment.body, ' /analyze') ||
       contains(github.event.comment.body, ' /test') ||
       contains(github.event.comment.body, ' /security') ||
       contains(github.event.comment.body, ' /explain') ||
       contains(github.event.comment.body, ' /triage') ||
       contains(github.event.comment.body, ' /help') ||
       contains(github.event.comment.body, ' /design') ||
       contains(github.event.comment.body, ' /merge')) &&
      (github.event.comment.author_association == 'OWNER' ||
       github.event.comment.author_association == 'MEMBER' ||
       github.event.comment.author_association == 'COLLABORATOR')
    uses: lenaxia/ai-workflows/.github/workflows/ai-comment.yml@v0.2.0
    secrets: inherit
    with:
      version: v0.2.0
      project_name: myrepo
```

**Self-hosted runners:** all three reusable workflows accept an optional `runs_on`
input (defaults to `ubuntu-latest`). If your repo runs CI on a self-hosted runner
label (e.g. gokore uses `gokore-runner`), pass it so AI jobs stay on your runner
instead of silently moving to GitHub-hosted runners:

```yaml
    with:
      version: v0.2.0
      project_name: myrepo
      runs_on: gokore-runner
```

### 4. Set up secrets and variables in the consumer repo

| Setting | Type | Purpose |
|---|---|---|
| `OPENAI_API_KEY` | secret | LLM API key |
| `OPENAI_API_BASE` | secret | LLM API base URL |
| `OPENAI_MODEL` | variable | Model name (e.g. `gpt-4o`) |

These are inherited by the reusable workflow via `secrets: inherit`. No
`AI_WORKFLOWS_PIN` variable is needed — the pin is hardcoded in each
workflow file's `uses:` and `version:` lines.

### 5. Create the prompts directory

The consumer needs `.github/prompts/` with at least `context.md`,
`core-rules.md`, and the command prompts (`pr-review.md`,
`issue-responder.md`, `fix.md`, `implement.md`, etc.). These are
consumer-owned — either write them from scratch or render a starting set:

```bash
go build -o /tmp/ai-sync ./scripts/ai-sync
/tmp/ai-sync render --consumer myrepo --into /path/to/myrepo/.github/prompts
```

If you rendered a starting set, review every file — the templates are
goKore-derived and will contain project-specific references you need to fix.

### 6. Add `opencode.json` to the consumer repo

The reusable workflows invoke OpenCode CLI, which requires an
`opencode.json` at the consumer repo root. This file configures the LLM
provider (OpenAI-compatible endpoint via env vars) and bash command
permissions for the agent runner.

Use the canonical config from this repo's root as a starting point:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "autoupdate": false,
  "provider": {
    "openai-compatible": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Custom OpenAI-compatible endpoint",
      "options": {
        "baseURL": "{env:OPENAI_API_BASE}",
        "apiKey": "{env:OPENAI_API_KEY}"
      },
      "models": {
        "default": {
          "name": "{env:OPENAI_MODEL}",
          "id": "{env:OPENAI_MODEL}"
        }
      }
    }
  },
  "permission": {
    "*": "allow",
    "bash": {
      "*": "deny",
      "git*": "allow",
      "gh*": "allow",
      "mkdir*": "allow",
      "cat*": "allow",
      "ls*": "allow",
      "pwd": "allow",
      "jq*": "allow"
    }
  }
}
```

Customize the `bash` allowlist for the consumer's toolchain. For example:

| Consumer | Extra bash allows | Why |
|----------|-------------------|-----|
| `gokore` | `go*`, `make*` | Go builds + Makefile targets |
| `LLMSafeSpaces` | `go*` | Go controller builds/tests |
| `containers` | `docker*`, `task*` | Docker buildx + Taskfile for image builds |

The deny-by-default bash policy (`"*": "deny"` inside `bash`) prevents the
agent from running arbitrary commands. Only explicitly allowed patterns run.
Add the minimum set of commands the agent needs for that repo's build and
test cycle.

## Rendering locally

```bash
go build -o /tmp/ai-sync ./scripts/ai-sync
/tmp/ai-sync list                          # list configured consumers
/tmp/ai-sync render --consumer gokore --into /tmp/out  # render one consumer
/tmp/ai-sync render --all --into /tmp/out              # render all consumers
/tmp/ai-sync diff --consumer gokore --into /path/to/gokore/.github/prompts  # show changes
```

## Testing

```bash
go test ./...                 # all tests (router + renderer)
go test ./tests/gharouter/... # router tests only
go test ./tests/aisync/...    # renderer tests only
```

The router tests (94 subtests) exercise `route-command.sh` end-to-end:
command detection, prefix-collision prevention (`/testing` ≠ `/test`),
`--no-merge` hold behavior, NOTE extraction, `/ai` context branching,
and prompt assembly ordering.

## Propagation

When a new version is tagged (`git tag v0.2.0`), `propagate.yml`:
1. Renders every consumer's prompt files using the new templates
2. **Bumps the `@<tag>` and `version:` in each consumer's workflow files** via
   sed (the `uses:` ref is a hardcoded literal — there is no `vars` indirection)
3. Opens a sync PR in each consumer repo with both changes (prompts + pin bump)

The PRs are NOT auto-merged — review each sync. Consumers can enable
auto-merge individually if they choose.

To roll back: revert the sync PR. Both the prompt files and the `@<tag>` pin
revert together in a single commit.

## Lessons learned

Hard-won knowledge from onboarding rathena-client and TinyRSVP. Each of these
caused real failures in production. Read before adding a new consumer.

### 1. The `uses:` ref must be a hardcoded literal (no `vars`)

GitHub's [context-availability table][ctx] has **no entry for
`jobs.<job_id>.uses`** — no contexts are permitted in the reusable-workflow
ref. The `uses:` value must be a literal string:

```yaml
# BROKEN — fails validation on every push:
uses: lenaxia/ai-workflows/.github/workflows/ai-comment.yml@${{ vars.AI_WORKFLOWS_PIN }}

# CORRECT:
uses: lenaxia/ai-workflows/.github/workflows/ai-comment.yml@v0.2.0
```

Symptom: every push fails with `context "vars" is not allowed here`.

[ctx]: https://docs.github.com/en/actions/learn-github-actions/contexts#context-availability

### 2. Caller workflows MUST declare explicit `permissions:`

A reusable workflow's `GITHUB_TOKEN` permissions **cannot exceed the caller's**.
If the caller omits `permissions:` and the repo default is read-only, the
reusable workflow's `id-token: write` / `issues: write` requests are silently
downgraded and the job is rejected at startup.

```yaml
# REQUIRED — without this, repos with read-only defaults get startup_failure:
permissions:
  id-token: write
  contents: read        # or write for ai-comment (allows code changes)
  issues: write
  pull-requests: write
```

Symptom: `startup_failure`, zero jobs, "This run likely failed because of a
workflow file issue" — even though the workflow file is valid and the
reusable workflow resolves correctly. This is the hardest failure to debug
because it looks like a YAML syntax error but isn't.

### 3. Templates are goKore-derived

The prompt templates in `templates/prompts/` were extracted from goKore. They
contain goKore-specific content: rathena-client packet handling, hook events,
builders, `docs/07_WORK_LOG/`, etc. **Rendering them into a non-goKore repo
produces incorrect prompts.**

Consumer configs must fork all prompts unless the repo genuinely wants
goKore-derived defaults:

```yaml
forked:
  - context.md
  - core-rules.md
  - analyze.md
  # ... all command prompts
```

gokore is the only consumer that renders templates directly. All others
(LLMSafeSpaces, rathena-client, TinyRSVP) fork their prompts and use this repo
for workflow plumbing only (reusable workflows, router, footer, the
formal-blocking-review directive).

### 4. The `if:` filter stays in the caller

A called workflow's job-level `if:` against `github.event.comment.*` is not
reliably evaluated for `issue_comment` / `pull_request_review_comment` events.
The command-token filter and `author_association` check **must** live in the
caller, not the reusable workflow. Same for the `renovate[bot]` skip on
`pr-review.yml`.

### 5. Git identity must be configured on self-hosted runners

This org's self-hosted ubuntu runners don't ship a default `git user.name` /
`user.email`. Without explicit configuration, `git commit` from the OpenCode
step fails with `empty ident name ... not allowed`. All three reusable
workflows include a "Configure git identity" step to set
`github-actions[bot]`.

### 6. The `failure` conclusion on `issue-opened` is expected

`issue-opened.yml` intentionally has `contents: read` and
`persist-credentials: false` — it's designed for commenting, not pushing code.
When the AI decides to make code changes (e.g. implementing a requested
feature), `git push` fails with `could not read Username for 'https://github.com'`.
This is pre-existing behavior, not a bug. Code changes should go through
`/implement` or `/fix` via `ai-comment.yml` (which has `contents: write` and
`persist-credentials: true`).

### 7. `pr-review.yml` gates its job on review delivery, not opencode's exit code

The `Run OpenCode` step in `pr-review.yml` has `continue-on-error: true`, and a
follow-up `Verify review submitted` step is the source of truth for the job's
pass/fail. This is necessary because `anomalyco/opencode` runs an unconditional
`git push` at end-of-run (to persist its session/snapshot branch); under
`pr-review.yml`'s required `persist-credentials: false` (the workflow only
**reviews** code, never pushes it), that push dies with
`fatal: could not read Username for 'https://github.com'` and opencode exits 1.
Without `continue-on-error`, every legitimately-approved consumer PR showed a
permanently red `review / review` required check even though
`gh pr review --approve` had already succeeded.

The verify step queries `gh api .../pulls/N/reviews` for an `APPROVED` or
`CHANGES_REQUESTED` verdict by `github-actions[bot]` pinned to the PR HEAD's
`commit_id`. If opencode crashed before posting any verdict, the verify step
fails the job (real failure mode preserved). If opencode posted the verdict and
then died on its end-of-run push, the verify step passes — which is the
correct outcome: the review IS the deliverable, and it landed. See issue #17.

This pattern does **not** apply to `issue-opened.yml`: that workflow's
deliverable is a comment, not a review, so there is no equivalent single event
to gate on; the `failure` conclusion there remains expected (see #6 above).

`issue-opened.yml` and `ai-comment.yml` do not need the
`continue-on-error` + verify pattern: `ai-comment.yml` already uses
`persist-credentials: true` (so opencode's push succeeds), and
`issue-opened.yml`'s failure is the intended read-only pushback.

### 8. Keep the `.ai-workflows` pinned checkout out of the consumer git index

All three reusable workflows check out the pinned `lenaxia/ai-workflows` repo
**inside** the consumer worktree (`path: .ai-workflows`). This is forced by
`actions/checkout@v6`: it validates that `path` resolves under
`$GITHUB_WORKSPACE` and throws
`Repository path '...' is not under the GITHUB_WORKSPACE` otherwise, so
`${{ runner.temp }}/ai-workflows` is not an option.

That nested checkout carries its own `.git`. Without an exclude, opencode's
end-of-run `git add -A` sweeps it into the consumer index as a gitlink (a
submodule reference with no `.gitmodules` entry), and `actions/checkout`'s
post-job cleanup then emits
`fatal: No url found for submodule path '.ai-workflows' in .gitmodules` on every
run. Each workflow records `.ai-workflows/` in `.git/info/exclude` (local to
that checkout, never committed) right after the nested checkout so the
directory stays readable from disk but never enters the consumer's git index.
See issue #17.

### 9. `propagate.yml` degrades gracefully without `AI_WORKFLOWS_PAT`

`propagate.yml` opens cross-repo sync PRs in every consumer on tag push. Two
non-obvious things had been silently breaking it for three consecutive
releases (v0.2.2 / v0.2.3 / v0.2.4):

1. **Empty `AI_WORKFLOWS_PAT`.** Cross-repo operations (checkout of a private
   consumer, opening any cross-repo PR) require a PAT with `repo` + `workflow`
   scope, stored as `AI_WORKFLOWS_PAT`. When that secret is unset, the
   literal `token: ${{ secrets.AI_WORKFLOWS_PAT }}` resolves to an empty
   string, and `actions/checkout` fails with a generic
   `Input required and not supplied: token` — which looks like a workflow
   bug, not a missing secret. The fix routes the token through a
   `Resolve auth token` step that falls back to `GITHUB_TOKEN` when the PAT
   is empty. The fallback is enough for public-consumer checkout + render +
   diff, so an operator can at least see what WOULD change. The cross-repo
   PR step is gated on `steps.auth.outputs.mode == 'pat'`; when the PAT is
   absent, a dedicated `Skip sync PR (no PAT)` step emits a `::warning::`
   with the exact remediation (set the secret and re-run).

2. **Dogfood pin drift.** `propagate.yml`'s consumer matrix handles OTHER
   repos but historically did not bump THIS repo's own dogfood pins
   (`self-pr-review.yml`, `self-ai-comment.yml`, `consumers/ai-workflows.yaml`).
   They were stuck at v0.2.1 across v0.2.2 / v0.2.3 until PR #20 caught them
   up manually. A new `dogfood-bump` job now bumps them on every tag push and
   regenerates the rendered prompts (whose banner embeds the version), then
   opens a self-PR for review. It runs entirely on the default `GITHUB_TOKEN`
   (same-repo), so it works even when `AI_WORKFLOWS_PAT` is not set.

The token-fallback, PAT-gated PR step, skip-step-with-warning, and
dogfood-bump job are all locked by
`tests/workflows/workflow_invariants_test.go::TestPropagate*`.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Every push fails: `context "vars" is not allowed here` | `uses:` ref contains `${{ vars.* }}` | Hardcode the tag: `@v0.2.0` |
| `startup_failure`, zero jobs, "workflow file issue" | Caller missing `permissions:` block | Add `permissions:` matching the reusable workflow's needs |
| `startup_failure` only on one repo, same org | That repo's default workflow permissions are read-only | Add explicit `permissions:` to the caller (or set repo default to read-write) |
| `Run OpenCode` fails: `empty ident name` | Self-hosted runner lacks git identity | Already fixed in v0.1.2 (all reusable workflows set git identity) |
| `Run OpenCode` fails: `could not read Username` on **issue-opened** | `issue-opened.yml` has `contents: read` — opencode's end-of-run push has no creds (and rarely, the AI also tries to push) | Expected; use `/implement` or `/fix` for code changes (see Lessons Learned #6) |
| `Run OpenCode` fails: `could not read Username` on **pr-review** | Same end-of-run push failure, but the review itself was already posted via `gh pr review` | Already fixed in v0.2.4 — `Run OpenCode` has `continue-on-error: true` and the `Verify review submitted` step drives the job conclusion (see Lessons Learned #7). The bot comment opencode leaves on the PR ("`fatal: could not read Username...`") is cosmetic noise from opencode echoing the push failure as a comment and can be ignored. |
| `fatal: No url found for submodule path '.ai-workflows' in .gitmodules` during cleanup | The pinned `.ai-workflows` checkout was swept into the consumer index as a gitlink by opencode's end-of-run `git add -A` | Already fixed in v0.2.4 — every reusable workflow writes `.ai-workflows/` to `.git/info/exclude` after the nested checkout (see Lessons Learned #8) |
| `propagate.yml`: `Input required and not supplied: token` on `Checkout consumer` | `AI_WORKFLOWS_PAT` secret unset → `token: ${{ secrets.AI_WORKFLOWS_PAT }}` resolves to empty string → `actions/checkout` rejects it | Already fixed in v0.2.5 — token is resolved via a `Resolve auth token` step that falls back to `GITHUB_TOKEN` (see Lessons Learned #9). To enable cross-repo PRs, set `AI_WORKFLOWS_PAT` (PAT with `repo` + `workflow` scope) and re-run. |
| `propagate.yml`: dogfood pins (`self-pr-review.yml`, `self-ai-comment.yml`, `consumers/ai-workflows.yaml`) drift across releases | Pre-v0.2.5, `propagate.yml`'s matrix handled OTHER repos but not this repo's own dogfood pins | Already fixed in v0.2.5 — a new `dogfood-bump` job bumps them on every tag push and opens a self-PR (see Lessons Learned #9) |
| Prompts contain wrong project content after sync | Templates are goKore-derived; consumer didn't fork | Add the affected files to `forked:` in the consumer config |
| `Run OpenCode` fails: `couldn't find remote ref` | PR branch was deleted while the AI was still running | Don't merge+delete-branch while a review run is in progress |
| AI job runs on GitHub-hosted runner instead of self-hosted | Caller didn't pass `runs_on` (defaults to `ubuntu-latest`) | Pass `runs_on: <your-label>` in the caller's `with:` |

## Consumers

| Repo | Status | Notes |
|---|---|---|
| gokore | Active | Primary consumer; prompts are gokore-derived |
| LLMSafeSpaces | Active | Original source; core-rules uses shared spine + SOLID/Quality blocks |
| rathena-client | Active | Heavy per-repo rules; core-rules uses extensive blocks |
| TinyRSVP | Active | Plumbing-only consumer; all prompts forked (RSVP-specific, not gokore-derived) |
| containers | Active | Plumbing-only consumer; all prompts forked (container image builds, not gokore-derived) |
| talos-ops-prod | Active | Plumbing-only consumer; all prompts forked (Talos/Flux GitOps, SOPS, not gokore-derived) |
| synology-to-immich | Active | Plumbing-only consumer; all prompts forked (Synology Photos → Immich migration, PostgreSQL, NFS, not gokore-derived) |
