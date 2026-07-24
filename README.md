# lenaxia/ai-workflows

Central source of truth for AI-command workflows across `lenaxia/*` repos.

## What this repo does

Provides reusable GitHub Action workflows, prompt templates, and the
`route-command.sh` router that powers slash-command AI interactions
(`/fix`, `/implement`, `/review`, etc.) on issues and PRs.

Consumer repos (gokore, LLMSafeSpaces, rathena-client, TinyRSVP) reference
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
│   ├── gharouter/            #   route-command.sh tests (87 subtests)
│   └── aisync/               #   ai-sync renderer tests (7 tests)
├── opencode.json             # Canonical opencode config
└── go.mod
```

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

1. Create `consumers/<name>.yaml`:
   ```yaml
   name: myrepo
   version: v0.1.0
   vars:
     project_name: MyRepo
     rules_doc: README-LLM.md
   forked:
     - context.md
   blocks: []
   ```

2. Add `<name>` to the matrix in `.github/workflows/propagate.yml`.

3. In the consumer repo, add three caller workflows:
   ```yaml
   # .github/workflows/ai-comment.yml
   on:
     issue_comment: { types: [created] }
     pull_request_review_comment: { types: [created] }
   jobs:
     respond:
       if: |
         (startsWith(github.event.comment.body, '/ai') || ...) &&
         (github.event.comment.author_association == 'OWNER' || ...)
       uses: lenaxia/ai-workflows/.github/workflows/ai-comment.yml@${{ vars.AI_WORKFLOWS_PIN }}
       secrets: inherit
       with:
         version: ${{ vars.AI_WORKFLOWS_PIN }}
         project_name: myrepo
   ```

4. Set the `AI_WORKFLOWS_PIN` repo variable in the consumer to a tag or SHA.

5. Set the `OPENAI_API_KEY`, `OPENAI_API_BASE` secrets and `OPENAI_MODEL` variable.

6. Run `ai-sync render --consumer myrepo --into .github/prompts` to install prompts.

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

The router tests (87 subtests) exercise `route-command.sh` end-to-end:
command detection, prefix-collision prevention (`/testing` ≠ `/test`),
`--no-merge` hold behavior, NOTE extraction, `/ai` context branching,
and prompt assembly ordering.

## Propagation

When a new version is tagged (`git tag v0.2.0`), `propagate.yml`:
1. Renders every consumer's files using the new templates
2. Opens a sync PR in each consumer repo
3. Updates the `AI_WORKFLOWS_PIN` variable

PRs are not auto-merged — review each sync. Consumers can enable auto-merge
individually if desired.

## Consumers

| Repo | Status | Notes |
|---|---|---|
| gokore | Active | Primary consumer; prompts are gokore-derived |
| LLMSafeSpaces | Active | Original source; core-rules uses shared spine + SOLID/Quality blocks |
| rathena-client | Active | Heavy per-repo rules; core-rules uses extensive blocks |
| TinyRSVP | Active | Plumbing-only consumer; all prompts forked (RSVP-specific, not gokore-derived) |
