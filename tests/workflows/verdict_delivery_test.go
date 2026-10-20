// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Verdict-delivery invariants for the reusable workflows, locking the
// fallback matrix of ai-workflows #49 / #46 and owner ruling #6209846676:
//
//   - both pr-review.yml gates delegate to scripts/check-verdict.sh (the
//     two-channel gate: official review OR sha-pinned marker comment);
//   - the build-prompt and /review command paths carry the verdict-comment
//     fallback directive (approve-blocked reviewers must post the full
//     review as a marker comment, never nothing);
//   - the dead `TOKEN:` env of #49 round 1 can never return (the pinned
//     action forwards only USE_GITHUB_TOKEN; the CLI never reads TOKEN);
//   - a pre-flight (scripts/author-permission.sh, same permission source as
//     the CLI's assertPermissions, fail-open) gates the LLM runs so
//     non-collaborator PR authors get an instant explained failure instead
//     of two doomed ~23s attempts;
//   - ai-comment.yml runs the same delivery gates for /review commands on
//     PRs (the outage-recovery path — without it a /review can conclude
//     green with zero verdict delivered, the silent-success cell).
//
// Run: go test ./tests/workflows/...
package workflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrReviewGatesUseCheckVerdictScript asserts both delivery gates call
// the shared two-channel script instead of review-only inline jq.
func TestPrReviewGatesUseCheckVerdictScript(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "pr-review.yml")

	for _, name := range []string{"Check verdict delivered", "Verify review submitted"} {
		block := stepBlock(body, name)
		if block == "" {
			t.Fatalf("pr-review.yml: no '%s' step found", name)
		}
		if !strings.Contains(block, "check-verdict.sh") {
			t.Errorf("pr-review.yml: '%s' must invoke scripts/check-verdict.sh (two-channel gate: review OR sha-pinned marker comment)", name)
		}
	}

	// The verify gate must fail the job exactly when delivered == 'false'
	// (neither channel could post) — never on channel=comment.
	verify := stepBlock(body, "Verify review submitted")
	if !strings.Contains(verify, "delivered") || !strings.Contains(verify, "exit 1") {
		t.Errorf("pr-review.yml: 'Verify review submitted' must exit 1 when delivered reports false")
	}
}

// TestPrReviewSalvageUpgradesMarkerComments: the first salvage pass must run
// whenever the verdict was NOT delivered as an official review
// (delivered != 'review') — including delivered=comment, so a marker
// comment is still upgraded to an official review whenever the platform
// allows it. Gating on == 'false' would strand comment-channel verdicts.
func TestPrReviewSalvageUpgradesMarkerComments(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "pr-review.yml")
	block := stepBlock(body, "Salvage dumped verdict")
	if block == "" {
		t.Fatal("pr-review.yml: no 'Salvage dumped verdict' step")
	}
	got := stepIfDirective(block)
	if !strings.Contains(got, `delivered != 'review'`) {
		t.Errorf("pr-review.yml: salvage must gate on delivered != 'review' (upgrade marker comments too), got %q", got)
	}
}

// TestPrReviewPromptCarriesFallbackDirective: the workflow-appended prompt
// must instruct the reviewer to fall back to a marker verdict comment when
// GitHub refuses the review event. The append rides the workflow itself so
// consumer-forked prompts cannot omit it.
func TestPrReviewPromptCarriesFallbackDirective(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "pr-review.yml")
	build := stepBlock(body, "Build prompt")
	if build == "" {
		t.Fatal("pr-review.yml: no 'Build prompt' step")
	}
	for _, want := range []string{"ai-workflows-verdict:", "422"} {
		if !strings.Contains(build, want) {
			t.Errorf("pr-review.yml: 'Build prompt' must carry the verdict-delivery fallback directive (%q missing)", want)
		}
	}
}

// TestPrReviewNoStepTokenEnv restores and hardens #49 round 1's ruling with
// the live-run evidence (LLMSafeSpaces run 39373330689): a step-level TOKEN
// env on the Run OpenCode steps is DEAD CODE — the composite action's inner
// "Run opencode" step declares its own env: block, which DROPS the calling
// step's env entirely (TOKEN:*** appeared in sibling steps' dumps but never
// in the CLI step's, while GITHUB_ENV-exported OPENCODE_PROMPT reached the
// CLI). The trusted-bot bypass therefore rides GITHUB_ENV from the authmode
// step (scripts/trusted-bot-auth.sh). A TOKEN: line here would mislead
// readers into thinking the bypass is wired when it cannot work.
func TestPrReviewNoStepTokenEnv(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "pr-review.yml")
	for _, name := range []string{"Run OpenCode", "Run OpenCode (bounded retry)"} {
		block := stepBlock(body, name)
		if block == "" {
			t.Fatalf("pr-review.yml: no '%s' step", name)
		}
		for _, line := range strings.Split(block, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "TOKEN:") {
				t.Errorf("pr-review.yml: '%s' must not set a step TOKEN: env — the composite action's inner step drops caller step env (dead code; the bypass rides GITHUB_ENV from trusted-bot-auth.sh — #49 r1 + run 39373330689)", name)
			}
		}
	}
}

// TestPrReviewPreflightAuthorPermission: the pre-flight step
// (scripts/author-permission.sh) must exist and both LLM runs must be
// gated on its output. The pre-flight reads the SAME permission source as
// the CLI's assertPermissions and fails OPEN, so the gate is
// `!= 'false'` — an unset output (crashed/skipped pre-flight) proceeds to
// the run exactly as pre-fix behavior, with the CLI assert still enforcing.
func TestPrReviewPreflightAuthorPermission(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "pr-review.yml")

	preflight := stepBlock(body, "Pre-flight author permission")
	if preflight == "" {
		t.Fatal("pr-review.yml: no 'Pre-flight author permission' step — non-collaborator PR authors burn two doomed LLM attempts before a bare red")
	}
	if !strings.Contains(preflight, "author-permission.sh") {
		t.Errorf("pr-review.yml: pre-flight must invoke scripts/author-permission.sh")
	}
	if strings.Contains(preflight, "continue-on-error: false") {
		// fail-open lives INSIDE the script; a step-level hard failure would
		// red the job before any review attempt on transient API errors.
		t.Errorf("pr-review.yml: pre-flight must not hard-fail the job (fail-open is in-script)")
	}

	for _, name := range []string{"Run OpenCode", "Run OpenCode (bounded retry)"} {
		block := stepBlock(body, name)
		if block == "" {
			t.Fatalf("pr-review.yml: no '%s' step", name)
		}
		got := stepIfDirective(block)
		if !strings.Contains(got, "steps.preflight.outputs.author_allowed != 'false'") {
			t.Errorf("pr-review.yml: '%s' must gate on steps.preflight.outputs.author_allowed != 'false' (fail-open), got %q", name, got)
		}
	}

	// The blocked path must leave an explanatory comment (recovery: a
	// member posts /review) — never a bare red with no PR-visible trace.
	blocked := stepBlock(body, "Report unreviewable author")
	if blocked == "" {
		t.Fatal("pr-review.yml: no 'Report unreviewable author' step — the blocked cell needs a PR-visible explanation")
	}
	if !strings.Contains(blocked, "author_allowed == 'false'") || !strings.Contains(blocked, "gh pr comment") {
		t.Errorf("pr-review.yml: 'Report unreviewable author' must be gated on author_allowed == 'false' and post a PR comment")
	}
}

// TestAiCommentReviewTailGates: /review commands on PRs (the outage-
// recovery path — pull_request events can be lost, comment events deliver)
// must run the same delivery gates as the pull_request workflow. Without
// this tail a /review that fails to submit concludes green: the
// silent-success cell of ai-comment.yml.
func TestAiCommentReviewTailGates(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "ai-comment.yml")

	build := stepBlock(body, "Build prompt")
	if build == "" {
		t.Fatal("ai-comment.yml: no 'Build prompt' step")
	}
	for _, want := range []string{"COMMAND=", "PR_HEAD_SHA", "PR_CONTEXT"} {
		if !strings.Contains(build, want) {
			t.Errorf("ai-comment.yml: 'Build prompt' must expose %s to GITHUB_OUTPUT for the /review tail gates", want)
		}
	}

	check := stepBlock(body, "Check verdict delivered")
	if check == "" {
		t.Fatal("ai-comment.yml: no 'Check verdict delivered' step for /review — silent-success cell")
	}
	got := stepIfDirective(check)
	if !strings.Contains(got, "COMMAND == '/review'") || !strings.Contains(got, "PR_CONTEXT == 'true'") {
		t.Errorf("ai-comment.yml: check gate must target /review on PRs, got %q", got)
	}
	if !strings.Contains(check, "check-verdict.sh") {
		t.Errorf("ai-comment.yml: 'Check verdict delivered' must invoke scripts/check-verdict.sh")
	}

	salvage := stepBlock(body, "Salvage dumped verdict")
	if salvage == "" || !strings.Contains(salvage, "salvage-verdict.sh") {
		t.Fatal("ai-comment.yml: /review tail must attempt salvage before failing")
	}

	verify := stepBlock(body, "Verify review submitted")
	if verify == "" {
		t.Fatal("ai-comment.yml: no 'Verify review submitted' step for /review — no delivery guarantee")
	}
	if !strings.Contains(verify, "check-verdict.sh") {
		t.Errorf("ai-comment.yml: 'Verify review submitted' must invoke scripts/check-verdict.sh")
	}
	gotVerify := stepIfDirective(verify)
	if !strings.Contains(gotVerify, "COMMAND == '/review'") || !strings.Contains(gotVerify, "PR_CONTEXT == 'true'") {
		t.Errorf("ai-comment.yml: verify gate must target /review on PRs, got %q", gotVerify)
	}
}

// TestAiCommentReviewTailResolvesPRNumberForReviewCommentEvents pins the F1
// regression (review round 2): pull_request_review_comment payloads carry
// `comment` + `pull_request` and NO `issue` key, so a bare
// github.event.issue.number resolves empty there — the tail's PR_NUMBER and
// the build step's head-SHA lookup both break, and the verify gate false-reds
// even when a verdict was delivered (violating "red is reserved for neither
// channel could post"). Every PR-number reference in the tail path must use
// the issue||pull_request fallback the concurrency group already uses.
func TestAiCommentReviewTailResolvesPRNumberForReviewCommentEvents(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "ai-comment.yml")

	build := stepBlock(body, "Build prompt")
	if build == "" {
		t.Fatal("ai-comment.yml: no 'Build prompt' step")
	}
	if !strings.Contains(build, "ISSUE_NUMBER: ${{ github.event.issue.number || github.event.pull_request.number }}") {
		t.Errorf("ai-comment.yml: Build prompt ISSUE_NUMBER must fall back to github.event.pull_request.number — "+
			"pull_request_review_comment payloads have no `issue` key, so the head-SHA lookup and the whole "+
			"/review tail see an empty PR number and false-red despite a delivered verdict")
	}

	for _, name := range []string{"Check verdict delivered", "Salvage dumped verdict", "Verify review submitted"} {
		block := stepBlock(body, name)
		if block == "" {
			t.Fatalf("ai-comment.yml: no '%s' step", name)
		}
		if !strings.Contains(block, "PR_NUMBER: ${{ github.event.issue.number || github.event.pull_request.number }}") {
			t.Errorf("ai-comment.yml: '%s' PR_NUMBER must fall back to github.event.pull_request.number (F1)", name)
		}
	}
}

// TestVerifyGatesParseCheckVerdictContract cross-pins the F2 contract: both
// verify steps extract the channel from check-verdict.sh's stdout with
// `sed -n 's/^Verdict delivery channel: .../p'` — the script's emit prefix
// and the gates' sed pattern must not drift independently (a reword alone
// would false-red every run fleet-wide).
func TestVerifyGatesParseCheckVerdictContract(t *testing.T) {
	const emitPrefix = "Verdict delivery channel: "
	root := invRoot(t)

	script, err := os.ReadFile(filepath.Join(root, "scripts", "check-verdict.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), `echo "Verdict delivery channel: `) {
		t.Errorf("scripts/check-verdict.sh must keep the exact emit prefix %q — both verify gates parse it", emitPrefix)
	}

	for _, wf := range []string{"pr-review.yml", "ai-comment.yml"} {
		body := readWorkflowFile(t, root, wf)
		verify := stepBlock(body, "Verify review submitted")
		if verify == "" {
			t.Fatalf("%s: no 'Verify review submitted' step", wf)
		}
		if !strings.Contains(verify, "s/^Verdict delivery channel: \\([a-z]*\\).*/\\1/p") {
			t.Errorf("%s: verify gate's sed must keep parsing the exact 'Verdict delivery channel: <channel>' prefix emitted by check-verdict.sh", wf)
		}
	}
}
