// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"strings"
	"testing"
)

// TestPrReviewDebounceWired pins lever 4 of the PR-review runtime work
// (LLMSafeSpaces #1763 comment 6309172907, owner ruling: 10-minute quiet
// window ONLY — the 20-45-minute variants were rejected because they trade
// away per-push verdict coverage). The debounce is a quiet window: the
// reusable workflow waits debounce_minutes before doing ANY work; a newer
// push during the window cancels the waiting run (per-PR concurrency,
// cancel-in-progress) and the fresh run restarts the timer. Event-driven
// simulation on #1763's 31 runs: verdict-neutral at 10 minutes (12/12
// verdicts retained), ~21% of review compute saved — the mid-flight
// supersession burns (17/31 runs, 5.1h wall) never start.
//
// Load-bearing details pinned here:
//   - the input exists, is a number, defaults to 10, and documents 0=off;
//   - the sleep step is the FIRST step of the review job (a superseding
//     push must cancel a sleep, not a half-done pass — that ordering is
//     the entire saving);
//   - the step is gated off at 0 (disabled debounce = zero overhead);
//   - the arithmetic converts minutes to seconds exactly once (a raw
//     `sleep ${DEBOUNCE_MINUTES}` would wait 10 SECONDS — a silent no-op
//     debounce);
//   - the per-PR concurrency group carries cancel-in-progress: true (the
//     restart-the-timer mechanism; without it the window just delays
//     every review serially).
func TestPrReviewDebounceWired(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "pr-review.yml")

	// Input contract.
	for _, required := range []string{
		"debounce_minutes:",
		"type: number",
		"default: 10",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("pr-review.yml: debounce input missing %q — the quiet-window contract (default 10, overridable per caller) is gone", required)
		}
	}

	// First-step requirement.
	debounce := stepBlock(body, "Debounce (quiet window)")
	if debounce == "" {
		t.Fatal("pr-review.yml: no 'Debounce (quiet window)' step — lever 4 (verdict-neutral 21% compute saving) is gone")
	}
	stepsIdx := strings.Index(body, "    steps:")
	if stepsIdx < 0 {
		t.Fatal("pr-review.yml: no steps: block found")
	}
	between := body[stepsIdx : strings.Index(body[stepsIdx:], "- name: Debounce (quiet window)")+stepsIdx]
	if strings.Contains(between, "- name:") {
		t.Errorf("pr-review.yml: 'Debounce (quiet window)' must be the FIRST step — a step before it burns work that a superseding push would cancel mid-flight (the entire saving depends on cancelling a sleep, not a pass)")
	}

	// 0 disables; minutes->seconds conversion happens exactly once.
	if !strings.Contains(debounce, "inputs.debounce_minutes > 0") {
		t.Errorf("pr-review.yml: debounce step must be gated on inputs.debounce_minutes > 0 — 0 must disable the window with zero overhead")
	}
	if !strings.Contains(debounce, "* 60") {
		t.Errorf("pr-review.yml: debounce sleep must convert minutes to seconds (* 60) — a raw sleep of the input would wait 10 SECONDS (silent no-op debounce)")
	}

	// Lever-4/PR-#69 coupling: the sleep step reports the window it slept
	// (debounce_seconds) and the check-verdict step forwards it, so the
	// structural-failure retry gate keeps measuring the first pass's own
	// lifetime. Without the subtraction, the default 10-minute window makes
	// every structurally-doomed first pass read >=10 run_minutes and the
	// >=3 gate never fires — the exact doomed-retry waste PR #69 exists to
	// stop, silently re-opened by lever 4 (adversarial-verify finding).
	if !strings.Contains(debounce, "id: debounce") {
		t.Errorf("pr-review.yml: debounce step must carry id: debounce — check-verdict consumes steps.debounce.outputs.debounce_seconds")
	}
	if !strings.Contains(debounce, `debounce_seconds=${seconds}`) {
		t.Errorf("pr-review.yml: debounce step must emit debounce_seconds to GITHUB_OUTPUT — check-verdict.sh subtracts it from run_minutes (the PR #69 gate coupling)")
	}
	checkStep := stepBlock(body, "Check verdict delivered")
	if checkStep == "" {
		t.Fatal("pr-review.yml: no 'Check verdict delivered' step found")
	}
	if !strings.Contains(checkStep, "DEBOUNCE_SECONDS: ${{ steps.debounce.outputs.debounce_seconds }}") {
		t.Errorf("pr-review.yml: 'Check verdict delivered' must pass DEBOUNCE_SECONDS from the debounce step — without it the retry gate measures the quiet window, not the pass")
	}

	// Restart-the-timer mechanism: per-PR concurrency with cancel-in-progress.
	concIdx := strings.Index(body, "concurrency:")
	if concIdx < 0 {
		t.Fatal("pr-review.yml: no concurrency block — the debounce cannot restart its timer without cancel-in-progress cancellation")
	}
	concBlock := body[concIdx : concIdx+400]
	if !strings.Contains(concBlock, "github.event.pull_request.number") {
		t.Errorf("pr-review.yml: concurrency group must key on the PR number (per-PR window), got:\n%s", concBlock)
	}
	if !strings.Contains(concBlock, "cancel-in-progress: true") {
		t.Errorf("pr-review.yml: concurrency must set cancel-in-progress: true — a newer push must cancel the waiting run and restart the timer, not queue behind it")
	}
}
