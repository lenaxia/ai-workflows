# Project context: lenaxia/ai-workflows

## What this repo is

Central source of truth for AI-command workflows across `lenaxia/*` repos. It
provides reusable GitHub Action workflows, prompt templates, and the
`route-command.sh` router that powers slash-command AI interactions (`/fix`,
`/implement`, `/review`, etc.) on issues and PRs.

Consumer repos (gokore, LLMSafeSpaces, rathena-client, TinyRSVP) reference this
repo via pinned reusable workflows. This repo also dogfoods itself: it consumes
its own reusable workflows via `.github/workflows/self-*.yml` callers.

## Layout

```
.github/workflows/   Reusable workflows (workflow_call) + CI + propagation
  ai-comment.yml       Called by consumers for /ai, /fix, etc.
  issue-opened.yml     Called when issues are opened
  pr-review.yml        Called on PR open/synchronize
  propagate.yml        On tag push: render + open PRs to consumers
  test-router.yml      Runs route-command.sh tests on every push/PR
  self-pr-review.yml   Dogfood caller: runs pr-review on THIS repo's PRs
.github/prompts/      Rendered prompt files (consumed by the workflows above)
scripts/
  route-command.sh    Slash-command router (single source of truth)
  post-footer.sh      Once-per-thread footer dedup
  pre-commit          Consumer-side hook (rejects edits to managed files)
  ai-sync/            Renderer: templates + consumer config -> files
templates/prompts/    Prompt templates (Go text/template)
consumers/            Per-repo configs + override blocks
tests/                Regression tests (gharouter + aisync)
```

## Conventions

- **Reusable workflows are `workflow_call`-only.** Their triggers (`pull_request`,
  `issue_comment`) and `if:` filters live in the CALLER, never the reusable
  workflow (see README Lessons Learned #4).
- **The `uses:` ref is a hardcoded tag literal** (no `${{ vars.* }}` — GitHub
  rejects it; see Lessons Learned #1).
- **Templates are goKore-derived.** Non-gokore consumers fork the command prompts.
  This repo renders only `pr-review.md`, `core-rules.md`, `commands-footer.md`
  (see `consumers/ai-workflows.yaml`).
- **Bash router logic has regression tests** (`tests/gharouter`). Any change to
  `route-command.sh` routing/hold detection must be accompanied by tests.

## How to test

```bash
go test ./...                 # router (94 subtests) + renderer (7 tests)
go test ./tests/gharouter/... # router tests only
go test ./tests/aisync/...    # renderer tests only
```

A passing `go test ./...` is required before merge. Live workflow behavior is
validated by dogfooding: opening a PR triggers `self-pr-review.yml`, which runs
the real AI reviewer.
