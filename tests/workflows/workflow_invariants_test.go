// workflow_invariants_test.go guards cross-cutting runtime invariants of the
// reusable workflow files in .github/workflows/ that the route-command /
// ai-sync test suites cannot reach.
//
// These tests lock in the fixes for issue #17:
//
//   - pr-review.yml must tolerate opencode's end-of-run push failure (which
//     always happens under persist-credentials: false) and gate the job's
//     conclusion on whether a verdict was actually posted — not on
//     opencode's exit status. Without this, every legitimately-approved
//     consumer PR shows a permanently red `review / review` required check.
//
//   - Every reusable workflow that does a nested `path: .ai-workflows`
//     checkout must keep that checkout out of the consumer repo's git
//     index. The nested checkout carries its own .git; without an exclude,
//     opencode's end-of-run `git add -A` sweeps it into the consumer index
//     as a gitlink, and actions/checkout's post-job cleanup then emits
//     `fatal: No url found for submodule path '.ai-workflows' in
//     .gitmodules`.
//
// Run: go test ./tests/workflows/...
package workflows

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// invRoot resolves the repository root relative to this test file. Named
// distinctly from repoRoot (pins_test.go) and workflowRoot
// (pin_consistency_test.go, prompt_*.go) so all four helpers can co-exist
// in package workflows without a duplicate-symbol collision (see the note
// in prompt_coverage_test.go that introduced the disambiguation pattern).
func invRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	if _, err := os.Stat(filepath.Join(root, ".github", "workflows")); err != nil {
		t.Fatalf(".github/workflows not found at %s: %v", root, err)
	}
	return root
}

// readWorkflowFile returns the content of a workflow file under
// .github/workflows/, failing the test if it is missing.
func readWorkflowFile(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// stepBlock returns the YAML body of the step whose `- name:` header at
// indent 6 matches the given name, up to (but excluding) the next such
// header. Returns "" if no such step exists. This is a deliberately small
// scanner: the workflow files in this repo all use two-space indentation
// and a single `- name:` per step at column 6.
func stepBlock(body, name string) string {
	header := "\n      - name: " + name
	start := strings.Index(body, header)
	if start < 0 {
		return ""
	}
	rest := body[start+len(header):]
	nextIdx := strings.Index(rest, "\n      - name:")
	if nextIdx < 0 {
		// Last step in the file: the block runs to end-of-file.
		return rest
	}
	return rest[:nextIdx]
}

// TestPrReviewToleratesOpenCodePushFailure asserts pr-review.yml has both
// halves of the issue #17 Bug A fix:
//
//  1. The Run OpenCode step has `continue-on-error: true` so its
//     unconditional end-of-run `git push` (which always fails under
//     persist-credentials: false) does not mark the job FAILED.
//  2. A subsequent verify step, gated on `if: always()`, queries the PR
//     reviews API and fails the job iff no APPROVED/CHANGES_REQUESTED
//     verdict by github-actions[bot] was posted against the PR HEAD.
//
// Without (1), every approved consumer PR shows a permanently red required
// check. Without (2), a real opencode crash (no review posted) would be
// masked as a success. Both halves must remain together; this test fails
// if either is removed in isolation.
func TestPrReviewToleratesOpenCodePushFailure(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "pr-review.yml")

	runStep := stepBlock(body, "Run OpenCode")
	if runStep == "" {
		t.Fatal("pr-review.yml: no 'Run OpenCode' step found — workflow structure changed; update this test")
	}
	if !strings.Contains(runStep, "continue-on-error: true") {
		t.Errorf("pr-review.yml: 'Run OpenCode' step is missing `continue-on-error: true`.\n"+
			"opencode unconditionally runs `git push` at end-of-run; under "+
			"persist-credentials: false that push dies with `could not read "+
			"Username` and opencode exits 1, marking the job FAILED even when "+
			"the review was posted successfully. See issue #17 Bug A.")
	}

	verifyStep := stepBlock(body, "Verify review submitted")
	if verifyStep == "" {
		t.Fatalf("pr-review.yml: no 'Verify review submitted' step found.\n"+
			"Without it, continue-on-error on the Run OpenCode step would mask "+
			"real opencode crashes as successes. Both halves of the fix must "+
			"remain together. See issue #17 Bug A.")
	}

	// Each `want` is a substring that locks one essential property of the
	// verify step. Use raw strings so backslashes/quotes in the YAML
	// survive verbatim.
	for _, want := range []string{
		`if: always()`,                       // run even when Run OpenCode was tolerated as failure
		`gh api`,                             // query the reviews endpoint directly
		`github-actions[bot]`,                // scope to THIS bot's reviews, not a human's
		`APPROVED`,                           // accept approval as a delivered verdict
		`CHANGES_REQUESTED`,                  // accept blocking request-changes as a delivered verdict
		`.commit_id ==`,                      // pin the verdict to THIS run's HEAD (skip stale reviews from prior commits)
		`github.event.pull_request.head.sha`, // ...and that HEAD comes from the event, not a stale cache
	} {
		if !strings.Contains(verifyStep, want) {
			t.Errorf("pr-review.yml: 'Verify review submitted' step is missing %q.\n"+
				"This substring locks one of the properties that make the verify step "+
				"the source of truth for the job conclusion. See issue #17 Bug A.\n"+
				"Step body was:\n%s", want, verifyStep)
		}
	}
}

// TestPrReviewVerifyStepIsAlwaysReachable asserts the verify step uses
// `if: always()` so it still runs when the Run OpenCode step was tolerated
// as a failure. A `continue-on-error: true` opencode step whose follower
// step uses the default `if: success()` would skip the follower on every
// real-failure mode — defeating the point of having a verify step at all.
//
// This is a structural assertion the substring test above can't catch: a
// step can contain the literal "if: always()" in a comment while its real
// gating uses `success()`. The test fails if the verify step's actual
// gating line is missing or weakened.
func TestPrReviewVerifyStepIsAlwaysReachable(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "pr-review.yml")
	verifyStep := stepBlock(body, "Verify review submitted")
	if verifyStep == "" {
		t.Skip("no verify step — TestPrReviewToleratesOpenCodePushFailure already reported this")
	}

	// The `if:` line is the first non-comment, non-blank line after the
	// step's `name:` header. We don't try to parse YAML — we just assert
	// that an `if: always()` directive appears at the start of a line
	// (column-aligned with `env:` / `run:` etc.).
	for _, line := range strings.Split(strings.TrimPrefix(verifyStep, "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "", strings.HasPrefix(trimmed, "#"):
			continue
		case strings.HasPrefix(trimmed, "if:"):
			if !strings.Contains(trimmed, "always()") {
				t.Errorf("pr-review.yml: 'Verify review submitted' has `%s` instead of `if: always()` — "+
					"the step would be skipped when Run OpenCode is tolerated as failure, defeating "+
					"the verify-the-verdict safety net. See issue #17 Bug A.", trimmed)
			}
			return
		}
	}
	t.Errorf("pr-review.yml: 'Verify review submitted' has no `if:` directive at all — " +
		"it would default to `if: success()` and skip on every tolerated opencode failure. " +
		"See issue #17 Bug A.")
}

// TestReusableWorkflowsExcludeAiWorkflowsFromIndex asserts every reusable
// workflow that does a nested `path: .ai-workflows` checkout also records
// that path in `.git/info/exclude` so opencode's end-of-run `git add -A`
// cannot sweep the nested checkout (which carries its own .git) into the
// consumer index as a gitlink. Without the exclude, actions/checkout's
// post-job cleanup emits `fatal: No url found for submodule path
// '.ai-workflows' in .gitmodules` on every run. See issue #17 Bug B.
//
// Note: we cannot sidestep this by placing the nested checkout under
// ${{ runner.temp }} instead — actions/checkout@v6 enforces that `path`
// resolves under $GITHUB_WORKSPACE (src/input-helper.ts throws
// `Repository path '...' is not under the GITHUB_WORKSPACE`), so the
// exclude is the structural fix, not a workaround.
func TestReusableWorkflowsExcludeAiWorkflowsFromIndex(t *testing.T) {
	root := invRoot(t)
	for _, wf := range []string{"pr-review.yml", "ai-comment.yml", "issue-opened.yml"} {
		body := readWorkflowFile(t, root, wf)

		// Identify the nested checkout by its step name (more stable than
		// the `path:` literal, which could be renamed in a future refactor
		// — at which point this test should be revisited in lockstep).
		if !strings.Contains(body, "name: Checkout ai-workflows (pinned)") {
			t.Fatalf("%s: missing 'Checkout ai-workflows (pinned)' step — workflow structure changed; update this test", wf)
		}

		// Both halves of the exclude command must be present: the
		// `.ai-workflows/` argument (so the right path is excluded) and
		// the `>> .git/info/exclude` redirection (so it lands in the
		// consumer repo's local exclude, not stdout).
		if !strings.Contains(body, "'.ai-workflows/") ||
			!strings.Contains(body, ">> .git/info/exclude") {
			t.Errorf("%s: 'Checkout ai-workflows (pinned)' step is not accompanied by a "+
				"`.git/info/exclude` write for `.ai-workflows/`.\n"+
				"The nested checkout carries its own .git; without the exclude, opencode's "+
				"end-of-run `git add -A` will sweep it into the consumer index as a gitlink "+
				"and actions/checkout's post-job cleanup will emit `fatal: No url found for "+
				"submodule path '.ai-workflows' in .gitmodules`. See issue #17 Bug B.", wf)
		}
	}
}

// TestPrReviewVerifyStepNegativeCase is the hermetic counterpart to
// TestPrReviewToleratesOpenCodePushFailure: a hand-built workflow body that
// is missing either half of the fix must be flagged by stepBlock /
// substring scanning. This locks the regression guard independently of the
// live workflow file (mirroring the pattern in
// pins_test.go::TestActionPinsNegativeCase).
func TestPrReviewVerifyStepNegativeCase(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantPass  bool
		missingIn string
	}{
		{
			name: "Run OpenCode step lacks continue-on-error",
			body: `
      - name: Run OpenCode
        uses: anomalyco/opencode/github@0cf0294787322664c6d668fa5ab0a9ce26796f78
        with:
          prompt: foo
      - name: Verify review submitted
        if: always()
        run: gh api ...
`,
			wantPass:  false,
			missingIn: "continue-on-error",
		},
		{
			name: "Verify step missing entirely",
			body: `
      - name: Run OpenCode
        uses: anomalyco/opencode/github@0cf0294787322664c6d668fa5ab0a9ce26796f78
        with:
          prompt: foo
        continue-on-error: true
`,
			wantPass:  false,
			missingIn: "Verify review submitted",
		},
		{
			name: "Verify step exists but missing commit_id pin",
			body: `
      - name: Run OpenCode
        uses: anomalyco/opencode/github@0cf0294787322664c6d668fa5ab0a9ce26796f78
        with:
          prompt: foo
        continue-on-error: true
      - name: Verify review submitted
        if: always()
        run: |
          gh api repos/foo/bar/pulls/1/reviews
`,
			wantPass:  false,
			missingIn: ".commit_id ==",
		},
		{
			name: "complete fix in place",
			body: `
      - name: Run OpenCode
        uses: anomalyco/opencode/github@0cf0294787322664c6d668fa5ab0a9ce26796f78
        with:
          prompt: foo
        continue-on-error: true
      - name: Verify review submitted
        if: always()
        env:
          PR_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
        run: |
          gh api "repos/x/y/pulls/1/reviews" \
            --jq "[.[] | select(.user.login == \"github-actions[bot]\" and (.state == \"APPROVED\" or .state == \"CHANGES_REQUESTED\") and .commit_id == \"${PR_HEAD_SHA}\")] | length"
`,
			wantPass: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runStep := stepBlock(tc.body, "Run OpenCode")
			verifyStep := stepBlock(tc.body, "Verify review submitted")

			hasContinueOnError := strings.Contains(runStep, "continue-on-error: true")
			hasVerify := verifyStep != ""
			hasAllSubstrings := true
			for _, want := range []string{
				`if: always()`, `gh api`, `github-actions[bot]`,
				`APPROVED`, `CHANGES_REQUESTED`,
				`.commit_id ==`, `github.event.pull_request.head.sha`,
			} {
				if !strings.Contains(verifyStep, want) {
					hasAllSubstrings = false
					break
				}
			}
			pass := hasContinueOnError && hasVerify && hasAllSubstrings

			if pass != tc.wantPass {
				t.Errorf("expected wantPass=%v but got pass=%v (continue-on-error=%v, verify=%v, allSubstrings=%v); missingIn=%q",
					tc.wantPass, pass, hasContinueOnError, hasVerify, hasAllSubstrings, tc.missingIn)
			}
		})
	}
}

// ---- propagate.yml invariants ----
//
// propagate.yml is internal release infra (not a reusable workflow consumed
// by other repos), so the route-command / ai-sync test suites don't touch
// it. The tests below lock the invariants of the post-issue-#17-and-beyond
// rewrite:
//
//   - The consumer checkout must NOT pass secrets.AI_WORKFLOWS_PAT directly
//     to actions/checkout's `token:` field. When that secret is unset (the
//     default state for this repo), the empty string propagates to the
//     action and checkout fails with a generic "Input required and not
//     supplied: token", which is exactly what broke propagate.yml across
//     v0.2.2/v0.2.3/v0.2.4. The fix routes through a `Resolve auth token`
//     step whose output is the PAT-or-GITHUB_TOKEN fallback; the checkout
//     reads steps.auth.outputs.token instead.
//
//   - A `dogfood-bump` job must exist: it bumps THIS repo's own dogfood pin
//     sites (self-pr-review.yml, self-ai-comment.yml, consumers/ai-workflows.yaml)
//     on every tag push. Without it, those pins silently drift (they were
//     stuck at v0.2.1 across v0.2.2/v0.2.3 until PR #20 caught them up).
//
//   - The cross-repo PR step must be gated on the auth mode being 'pat':
//     GITHUB_TOKEN can read public consumer repos but cannot open PRs in
//     other repos, so attempting it would fail. A companion skip step emits
//     a visible ::warning:: instead.

// hasJob is a small helper that reports whether the YAML body declares a
// top-level job with the given name. It matches `  <name>:` at exactly two
// spaces of indentation (the indentation of a job key under `jobs:`).
func hasJob(body, name string) bool {
	for _, line := range strings.Split(body, "\n") {
		if line == "  "+name+":" {
			return true
		}
	}
	return false
}

// TestPropagateCheckoutDoesNotUseRawPAT asserts the consumer-matrix checkout
// step reads the token from the resolved auth step output, not directly from
// secrets.AI_WORKFLOWS_PAT. Passing the secret directly makes actions/checkout
// fail with "Input required and not supplied: token" the moment the secret is
// unset - which is the failure mode that broke every propagate run from
// v0.2.2 through v0.2.4.
func TestPropagateCheckoutDoesNotUseRawPAT(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")
	coStep := stepBlock(body, "Checkout consumer")
	if coStep == "" {
		t.Fatal("propagate.yml: no 'Checkout consumer' step found - workflow structure changed; update this test")
	}
	if strings.Contains(coStep, "token: ${{ secrets.AI_WORKFLOWS_PAT }}") {
		t.Errorf("propagate.yml: 'Checkout consumer' step passes secrets.AI_WORKFLOWS_PAT directly to actions/checkout's token field.\n" +
			"When that secret is unset (the default for this repo), the empty string propagates and checkout fails with " +
			"a generic 'Input required and not supplied: token' - the exact failure that broke propagate from v0.2.2 through v0.2.4. " +
			"The token must be resolved via a preceding 'Resolve auth token' step whose output is the PAT-or-GITHUB_TOKEN fallback, " +
			"and the checkout reads steps.auth.outputs.token.")
	}
	if !strings.Contains(coStep, "steps.auth.outputs.token") {
		t.Errorf("propagate.yml: 'Checkout consumer' step does not reference steps.auth.outputs.token.\n" +
			"The token must come from a 'Resolve auth token' step so an empty AI_WORKFLOWS_PAT degrades to the " +
			"GITHUB_TOKEN fallback instead of crashing the checkout.")
	}
}

// TestPropagateHasAuthTokenResolutionStep asserts the 'Resolve auth token'
// step exists and emits both the PAT and GITHUB_TOKEN env vars (so the shell
// fallback `if [ -n \"$PAT\" ]; then ... else $GT; fi` can work).
func TestPropagateHasAuthTokenResolutionStep(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")
	authStep := stepBlock(body, "Resolve auth token")
	if authStep == "" {
		t.Fatal("propagate.yml: no 'Resolve auth token' step found.\n" +
			"This step resolves AI_WORKFLOWS_PAT -> GITHUB_TOKEN so an empty PAT degrades gracefully " +
			"instead of crashing actions/checkout with 'Input required and not supplied: token'.")
	}
	for _, want := range []string{
		`PAT: ${{ secrets.AI_WORKFLOWS_PAT }}`,
		`GT: ${{ secrets.GITHUB_TOKEN }}`,
		`mode=pat`,
		`mode=github-token-fallback`,
	} {
		if !strings.Contains(authStep, want) {
			t.Errorf("propagate.yml: 'Resolve auth token' step is missing %q.\n"+
				"This substring locks one of the properties that make the token fallback work. "+
				"Step body was:\n%s", want, authStep)
		}
	}
}

// TestPropagatePRStepGatedOnPAT asserts the cross-repo 'Open sync PR' step
// is gated on steps.auth.outputs.mode == 'pat'. GITHUB_TOKEN cannot open PRs
// in other repos (even public ones), so attempting it without a PAT would
// fail; the gate ensures the skip step (which emits a ::warning::) runs
// instead.
func TestPropagatePRStepGatedOnPAT(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")
	prStep := stepBlock(body, "Open sync PR")
	if prStep == "" {
		t.Fatal("propagate.yml: no 'Open sync PR' step found - workflow structure changed; update this test")
	}
	// Pull the `if:` line out of the step block (first non-comment/
	// non-blank line starting with `if:`).
	for _, line := range strings.Split(strings.TrimPrefix(prStep, "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "if:") {
			if !strings.Contains(trimmed, "steps.auth.outputs.mode == 'pat'") {
				t.Errorf("propagate.yml: 'Open sync PR' if: line is `%s` but does not gate on steps.auth.outputs.mode == 'pat'.\n"+
					"Without that gate, the PR step would run with GITHUB_TOKEN (the fallback) and fail, since GITHUB_TOKEN "+
					"cannot open PRs in other repos. if: line was:\n%s", trimmed, trimmed)
			}
			return
		}
	}
	t.Errorf("propagate.yml: 'Open sync PR' step has no if: directive at all - it would run unconditionally.")
}

// TestPropagateHasSkipStepForMissingPAT asserts the graceful-degradation
// branch exists: when auth mode is not 'pat', a dedicated step emits a
// ::warning:: so the operator is told WHY no PR was opened and how to fix
// it (set AI_WORKFLOWS_PAT and re-run). Pre-fix, the missing-PAT path just
// crashed at checkout with a generic error.
func TestPropagateHasSkipStepForMissingPAT(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")
	skipStep := stepBlock(body, "Skip sync PR (no PAT)")
	if skipStep == "" {
		t.Fatal("propagate.yml: no 'Skip sync PR (no PAT)' step found.\n" +
			"This is the graceful-degradation branch: when AI_WORKFLOWS_PAT is not set, " +
			"the cross-repo PR step is skipped and this step emits a ::warning:: telling the operator " +
			"how to enable full propagation (set the PAT and re-run).")
	}
	for _, want := range []string{
		`steps.auth.outputs.mode != 'pat'`,
		`::warning::`,
		`AI_WORKFLOWS_PAT`,
	} {
		if !strings.Contains(skipStep, want) {
			t.Errorf("propagate.yml: 'Skip sync PR (no PAT)' step is missing %q.\n"+
				"Step body was:\n%s", want, skipStep)
		}
	}
}

// TestPropagateHasDogfoodBumpJob asserts the dogfood-bump job exists. It
// bumps THIS repo's own dogfood pin sites on every tag push so they don't
// drift (they were stuck at v0.2.1 across v0.2.2/v0.2.3 because propagate's
// consumer matrix only handles OTHER repos).
func TestPropagateHasDogfoodBumpJob(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")
	if !hasJob(body, "dogfood-bump") {
		t.Fatal("propagate.yml: no 'dogfood-bump' job found.\n" +
			"This job bumps this repo's own dogfood pin sites (self-pr-review.yml, " +
			"self-ai-comment.yml, consumers/ai-workflows.yaml) on every tag push. Without it, " +
			"those pins silently drift - they were stuck at v0.2.1 across v0.2.2/v0.2.3 until PR #20 " +
			"caught them up manually.")
	}
	// The job must touch all three pin sites guarded by
	// TestDogfoodPinsAreConsistent, plus regenerate rendered prompts guarded
	// by TestRenderedPromptsMatchTemplates.
	jobBlock := extractJobBlock(body, "dogfood-bump")
	if jobBlock == "" {
		t.Fatalf("propagate.yml: found 'dogfood-bump:' job key but could not extract its body")
	}
	for _, want := range []string{
		`consumers/ai-workflows.yaml`,
		`self-pr-review.yml`,
		`self-ai-comment.yml`,
		`ai-sync render`,
	} {
		if !strings.Contains(jobBlock, want) {
			t.Errorf("propagate.yml: 'dogfood-bump' job is missing %q.\n"+
				"This substring locks one of the dogfood-bump invariants. Job body was:\n%s",
				want, jobBlock)
		}
	}
}

// extractJobBlock returns the body of a top-level job (from `  <name>:` at
// two spaces of indentation up to the next top-level key or end-of-file).
// It's a deliberately small scanner paired with hasJob above.
func extractJobBlock(body, name string) string {
	header := "  " + name + ":"
	start := strings.Index(body, header+"\n")
	if start < 0 {
		// also accept the key being the last thing in the file with no trailing newline
		start = strings.Index(body, header)
		if start < 0 || start+len(header) != len(body) {
			return ""
		}
	}
	rest := body[start+len(header):]
	// The job body is everything indented at least 2 spaces (or blank lines),
	// up to the next top-level key (a non-blank line starting at column 0).
	var out strings.Builder
	for _, line := range strings.Split(rest, "\n") {
		if line == "" {
			out.WriteString(line + "\n")
			continue
		}
		if len(line) >= 2 && (line[0:2] == "  " || line[0] == ' ' || line[0] == '\t') {
			out.WriteString(line + "\n")
			continue
		}
		// First non-blank, non-indented line ends the job block.
		break
	}
	return out.String()
}
