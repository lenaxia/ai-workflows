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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// exactly two spaces of indentation up to the next sibling job key at the
// same indentation, or end-of-file). It's a deliberately small scanner
// paired with hasJob above.
//
// IMPORTANT boundary: the loop must terminate at the next line that is a
// YAML key at exactly two spaces of indent (e.g. `  propagate:` when
// extracting `dogfood-bump`), NOT at "any non-indented line" — because all
// job content (including the next sibling job's key) is indented relative
// to column 0, a column-0 terminator never fires and the helper would
// silently return the whole rest of the file. That made
// TestPropagateHasDogfoodBumpJob pass by accident (its substrings happened
// to also appear in the `propagate` body); isSiblingJobKey restores the
// precise scoping the doc comment always promised.
func extractJobBlock(body, name string) string {
	header := "  " + name + ":"
	start := strings.Index(body, header+"\n")
	if start < 0 {
		// also accept the key being the last thing in the file with no trailing newline
		if strings.HasSuffix(body, header) {
			start = len(body) - len(header)
		} else {
			return ""
		}
	}
	rest := body[start+len(header):]
	var out strings.Builder
	for _, line := range strings.Split(rest, "\n") {
		if isSiblingJobKey(line) {
			break
		}
		out.WriteString(line + "\n")
	}
	return out.String()
}

// isSiblingJobKey reports whether line is a YAML key at exactly two spaces
// of indentation matching the shape of a job name under `jobs:` (e.g.
// `  propagate:`, `  dogfood-bump:`). Used by extractJobBlock to find where
// one job body ends and the next sibling begins. It rejects:
//   - inline-valued keys like `  if: foo` (a job key has NO inline value;
//     its value is the indented block below)
//   - keys at any other indentation (0, 4, 6, ... spaces)
//   - job names containing characters outside [A-Za-z0-9_-] (this repo's
//     convention; narrows the match so a stray `  - name:` step header at
//     2 spaces — which never happens in well-formed YAML anyway — can't
//     terminate a block early)
func isSiblingJobKey(line string) bool {
	// Need at least "  x:" (4 chars) and exactly two leading spaces.
	if len(line) < 4 || line[0] != ' ' || line[1] != ' ' || line[2] == ' ' || line[2] == '\t' {
		return false
	}
	rest := line[2:]
	if !strings.HasSuffix(rest, ":") {
		return false
	}
	ident := strings.TrimSuffix(rest, ":")
	if ident == "" || strings.ContainsAny(ident, " \t") {
		// inline value present (e.g. "  if: foo") or empty key — not a job key
		return false
	}
	for _, r := range ident {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// TestExtractJobBlockBoundary is the hermetic regression test for the
// extractJobBlock helper itself. Before the isSiblingJobKey fix, the helper
// over-extracted (it never hit a column-0 line, so it returned the entire
// rest of the file), which made TestPropagateHasDogfoodBumpJob assert
// "substring appears in dogfood-bump-OR-propagate" instead of "in
// dogfood-bump" — a silent false-negative in a repo whose value
// proposition is catching workflow drift. This test pins the precise
// boundary with a hand-built fixture.
func TestExtractJobBlockBoundary(t *testing.T) {
	const fixture = `name: Test
on:
  push:
jobs:
  dogfood-bump:
    runs-on: ubuntu-latest
    steps:
      - name: Only-in-dogfood
        run: echo dogfood-marker
  propagate:
    runs-on: ubuntu-latest
    steps:
      - name: Only-in-propagate
        run: echo propagate-marker
`

	got := extractJobBlock(fixture, "dogfood-bump")
	if !strings.Contains(got, "dogfood-marker") {
		t.Errorf("extractJobBlock lost the dogfood-bump body: got %q", got)
	}
	if strings.Contains(got, "propagate-marker") {
		t.Errorf("extractJobBlock over-extracted: the propagate body leaked in. "+
			"got %q\nThis is the exact boundary bug the reviewer on PR #21 caught: "+
			"the helper must stop at the next sibling job key (`  propagate:`), "+
			"not at a column-0 line that never fires.", got)
	}
	if strings.Contains(got, "  propagate:") {
		t.Errorf("extractJobBlock did not terminate at the `  propagate:` sibling key. "+
			"got %q", got)
	}
}

// TestPropagatePRUsesResolvedBaseBranch asserts the 'Open sync PR' step
// does NOT hardcode `--base main`. Consumers may use a different default
// branch; the pre-fix code assumed `main` everywhere. The fix added a
// 'Resolve default branch' step that asks the API, and the PR step now
// passes `--base "${{ steps.base.outputs.branch }}"`.
//
// This test locks the fix the PR body claims but (per the PR #21 review)
// was originally not regression-guarded.
func TestPropagatePRUsesResolvedBaseBranch(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")
	prStep := stepBlock(body, "Open sync PR")
	if prStep == "" {
		t.Fatal("propagate.yml: no 'Open sync PR' step found - workflow structure changed; update this test")
	}

	// The literal "--base main" anywhere in the step is the regression
	// signature: it means the dynamic resolution was reverted in favor of
	// the hardcoded branch.
	if strings.Contains(prStep, "--base main") {
		t.Errorf("propagate.yml: 'Open sync PR' step contains a hardcoded `--base main`.\n" +
			"This assumes every consumer's default branch is `main`; the first consumer " +
			"whose default branch differs would get its sync PR opened against a nonexistent base. " +
			"Use `--base \"${{ steps.base.outputs.branch }}\"` driven by the 'Resolve default branch' step.")
	}

	if !strings.Contains(prStep, "steps.base.outputs.branch") {
		t.Errorf("propagate.yml: 'Open sync PR' step does not reference steps.base.outputs.branch.\n" +
			"The base branch must come from the 'Resolve default branch' step so consumers " +
			"whose default branch is not `main` are handled correctly.")
	}

	// And the resolver step itself must exist and consult the API.
	baseStep := stepBlock(body, "Resolve default branch")
	if baseStep == "" {
		t.Fatal("propagate.yml: no 'Resolve default branch' step found.\n" +
			"This step resolves the consumer's actual default branch via the API; without it, " +
			"steps.base.outputs.branch would be empty and the PR step would fail.")
	}
	if !strings.Contains(baseStep, "default_branch") || !strings.Contains(baseStep, "gh api") {
		t.Errorf("propagate.yml: 'Resolve default branch' step does not consult the GitHub API for the default branch.\n"+
			"Step body was:\n%s", baseStep)
	}
}

// TestPropagateBumpStepUsesMultiFileDiscovery asserts the 'Bump tag in
// consumer workflow files' step discovers the existing pin (OLD_TAG) by
// scanning multiple workflow files in order, not just ai-comment.yml. The
// pre-fix code grepped only ai-comment.yml; if a consumer lacked that file
// the variable came back empty and the whole bump silently no-op'd (the
// sed ran on no files).
//
// This test locks the fix the PR body claims but (per the PR #21 review)
// was originally not regression-guarded.
//
// Precision note: the step contains TWO `for wf in ...` loops — one for
// OLD_TAG discovery (grep) and one for sed application. Both reference the
// three workflow files, so a naive "step contains pr-review.yml" assertion
// passes even when discovery is reverted to ai-comment.yml-only (the
// application loop still has it). This test instead pins the exact
// discovery-loop header line, which is the one that actually feeds the
// grep. If a future refactor reorders the files, the test name tells the
// maintainer exactly what to update.
func TestPropagateBumpStepUsesMultiFileDiscovery(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")
	bumpStep := stepBlock(body, "Bump tag in consumer workflow files")
	if bumpStep == "" {
		t.Fatal("propagate.yml: no 'Bump tag in consumer workflow files' step found - workflow structure changed; update this test")
	}

	// The discovery loop is the `for wf in ...` line that precedes the
	// `OLD_TAG=` assignment. Pin its exact shape: all three consumer
	// workflow files in the discovery order. This is what makes OLD_TAG
	// discovery robust against consumers that lack ai-comment.yml.
	const discoveryLoopHeader = "for wf in .github/workflows/ai-comment.yml .github/workflows/pr-review.yml .github/workflows/issue-opened.yml; do"
	if !strings.Contains(bumpStep, discoveryLoopHeader) {
		t.Errorf("propagate.yml: 'Bump tag' step is missing the multi-file discovery loop header.\n"+
			"Expected to find: %q\n"+
			"A single-file discovery (ai-comment.yml only) would silently no-op the pin bump "+
			"for any consumer that lacks ai-comment.yml: OLD_TAG comes back empty, the `if [ -n \"$OLD_TAG\" ]` "+
			"guard fails, and sed runs on zero files. The fix discovers OLD_TAG from whichever of the three "+
			"workflow files the consumer actually has.\nStep body was:\n%s", discoveryLoopHeader, bumpStep)
	}

	// The discovery regex must accept BOTH pin shapes this repo's consumers
	// use: a 40-char SHA pin and a vX.Y.Z tag pin. Pre-fix the regex only
	// matched a narrower set.
	if !strings.Contains(bumpStep, "[0-9a-f]{40}") {
		t.Errorf("propagate.yml: 'Bump tag' step's OLD_TAG regex does not accept a 40-char SHA pin.\n" +
			"Consumers pinned by SHA (the repo convention) would not be discovered and the bump would no-op.")
	}
	if !strings.Contains(bumpStep, "v[0-9]+") {
		t.Errorf("propagate.yml: 'Bump tag' step's OLD_TAG regex does not accept a vX.Y.Z tag pin.\n" +
			"Consumers pinned by tag would not be discovered and the bump would no-op.")
	}
}

// TestPropagateWarningsHaveNoDoubleV asserts no ::warning:: or ::error::
// string in propagate.yml contains the pattern `v${{ steps.ver.outputs.v }}`
// or `v${NEW_TAG}` — both produce a literal `vvX.Y.Z` in the rendered
// warning because the version source already carries the `v` prefix
// (`steps.ver.outputs.v` = `${GITHUB_REF##*/}` = `v0.2.7`; `NEW_TAG` is
// sourced from the same output).
//
// This is a regression guard for the twin defects caught on PR #24's
// review: the dogfood-bump skip warning (line ~216) and the consumer-sync
// skip warning (line ~427) both had `v${...}`, producing `vv0.2.7` in
// real run logs. A single grep-level check catches both present and any
// future twins.
func TestPropagateWarningsHaveNoDoubleV(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")

	// `v\$\{\{ *steps\.ver\.outputs\.v` — matches `v${{ steps.ver.outputs.v }}`
	// with optional spaces after `{{` (GitHub Actions expression whitespace
	// is lenient). The literal `v` before `${{` is the defect: the
	// expression itself evaluates to `v0.2.7`, so the prefix produces
	// `vv0.2.7`.
	reVerExp := regexp.MustCompile("v\\$\\{\\{ *steps\\.ver\\.outputs\\.v")

	// `v\$\{NEW_TAG\}` — same defect in the dogfood-bump job's bash context
	// (NEW_TAG is a shell variable sourced from steps.ver.outputs.v).
	reNewTag := regexp.MustCompile("v\\$\\{NEW_TAG\\}")

	for _, line := range strings.Split(body, "\n") {
		if loc := reVerExp.FindStringIndex(line); loc != nil {
			t.Errorf("propagate.yml: line contains a double-v defect `v${{ steps.ver.outputs.v }}`:\n  %s\n"+
				"`steps.ver.outputs.v` already evaluates to `vX.Y.Z` (the tag ref includes the `v`); "+
				"the literal `v` prefix produces `vvX.Y.Z` in the rendered warning. Drop the leading `v`.",
				strings.TrimSpace(line))
		}
		if loc := reNewTag.FindStringIndex(line); loc != nil {
			t.Errorf("propagate.yml: line contains a double-v defect `v${NEW_TAG}`:\n  %s\n"+
				"NEW_TAG is sourced from steps.ver.outputs.v (which is `vX.Y.Z`); the literal `v` "+
				"prefix produces `vvX.Y.Z`. Drop the leading `v`.",
				strings.TrimSpace(line))
		}
	}
}

// TestPropagateMatrixConsumersHaveConfigFiles asserts every consumer named
// in the propagate matrix has a matching consumers/<name>.yaml config file.
// ai-sync render looks up the config by the exact consumer name passed on
// the CLI; a case mismatch (the matrix had "LLMSafeSpaces" but the file was
// "llmsafespaces.yaml") makes render fail with "no such file or directory"
// and turns that consumer's matrix job red. This test catches the mismatch
// at test time instead of at release time.
//
// GitHub repo cloning is case-insensitive (lenaxia/llmsafespaces resolves
// to LLMSafeSpaces), so the matrix entry should match the config-file
// convention (lowercase), not the repo's display-case.
func TestPropagateMatrixConsumersHaveConfigFiles(t *testing.T) {
	root := invRoot(t)
	body := readWorkflowFile(t, root, "propagate.yml")

	// Extract the matrix consumer list from the `consumer: [...]` line.
	// This is a deliberately narrow regex: the matrix in propagate.yml is a
	// single-line list, and the consumer key appears exactly once.
	re := regexp.MustCompile(`consumer:\s*\[([^\]]+)\]`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("propagate.yml: could not find `consumer: [...]` matrix line - workflow structure changed; update this test")
	}
	consumers := strings.Split(m[1], ",")
	if len(consumers) == 0 {
		t.Fatal("propagate.yml: matrix consumer list is empty")
	}

	for _, c := range consumers {
		name := strings.TrimSpace(c)
		if name == "" {
			continue
		}
		cfgPath := filepath.Join(root, "consumers", name+".yaml")
		if _, err := os.Stat(cfgPath); err != nil {
			// Check if a case-shifted version exists (the original bug).
			dir := filepath.Join(root, "consumers")
			entries, _ := os.ReadDir(dir)
			var lowercaseMatch string
			for _, e := range entries {
				if strings.EqualFold(e.Name(), name+".yaml") {
					lowercaseMatch = e.Name()
					break
				}
			}
			hint := ""
			if lowercaseMatch != "" {
				hint = fmt.Sprintf(" A case-shifted file exists at consumers/%s — change the matrix entry to %q to match (GitHub repo cloning is case-insensitive, so the clone still works).",
					lowercaseMatch, strings.ToLower(lowercaseMatch[:len(lowercaseMatch)-len(".yaml")]))
			}
			t.Errorf("propagate.yml: matrix consumer %q has no matching config file at consumers/%s.yaml.%s"+
				" ai-sync render looks up the config by the exact consumer name; a case mismatch makes render fail.",
				name, name, hint)
		}
	}
}

// TestPropagateDogfoodBumpPRGatedOnPAT asserts the dogfood-bump job gates
// its push/PR step on AI_WORKFLOWS_PAT being available. The job's commit
// modifies .github/workflows/self-*.yml (the dogfood pin sites); the
// GITHUB_TOKEN is POLICY-restricted from pushing workflow-file changes
// (no `permissions:` key overrides this — it's a GitHub-App-token-level
// restriction, not a scope issue), so only a PAT with `workflow` scope
// can push it.
//
// An earlier iteration of this fix tried to declare `workflows: write` in
// the job's permissions block. That key does not exist in the GITHUB_TOKEN
// permission set (the valid keys are actions, checks, contents,
// deployments, id-token, issues, packages, pages, pull-requests,
// repository-projects, security-events, statuses) and the unknown key made
// GitHub Actions reject the whole workflow with a "workflow file issue"
// startup failure — blocking EVERY propagate run (v0.2.6 included). This
// test now locks the correct shape: a Resolve auth token step + a PR step
// gated on mode == 'pat' + a Skip step for the no-PAT case.
func TestPropagateDogfoodBumpPRGatedOnPAT(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")
	jobBlock := extractJobBlock(body, "dogfood-bump")
	if jobBlock == "" {
		t.Fatal("propagate.yml: no 'dogfood-bump' job found - workflow structure changed; update this test")
	}

	// `workflows:` is NOT a valid GITHUB_TOKEN permission key. Its presence
	// causes a "workflow file issue" startup failure (GitHub rejects unknown
	// permission keys). This is the exact bug that blocked v0.2.6's
	// propagate run.
	if strings.Contains(jobBlock, "workflows: write") || strings.Contains(jobBlock, "workflows: read") {
		t.Errorf("propagate.yml: 'dogfood-bump' job declares a `workflows:` permission.\n" +
			"`workflows` is NOT a valid GITHUB_TOKEN permission key (valid keys: actions, " +
			"checks, contents, deployments, id-token, issues, packages, pages, " +
			"pull-requests, repository-projects, security-events, statuses). Its presence " +
			"makes GitHub Actions reject the whole workflow with a 'workflow file issue' " +
			"startup failure. The dogfood-bump commit touches .github/workflows/self-*.yml, " +
			"which the GITHUB_TOKEN is POLICY-restricted from pushing — no permissions key " +
			"overrides this. The push must use AI_WORKFLOWS_PAT (see the Resolve auth token " +
			"step + the mode == 'pat' gate on the Open dogfood-bump PR step).")
	}

	// The dogfood-bump job must have its own PAT-resolution pattern (the
	// consumer-matrix job has a separate Resolve auth token step). Confirm
	// the dogfood-bump block contains the mode=pat / mode=no-pat outputs.
	if !strings.Contains(jobBlock, "mode=pat") || !strings.Contains(jobBlock, "mode=no-pat") {
		t.Errorf("propagate.yml: 'dogfood-bump' job is missing a PAT-resolution step emitting mode=pat / mode=no-pat.\n" +
			"The push step must be gated on mode == 'pat' so the no-PAT case skips with a " +
			"::warning:: instead of failing the job.")
	}

	// The Open dogfood-bump PR step must be gated on mode == 'pat'.
	prStep := stepBlock(body, "Open dogfood-bump PR")
	if prStep == "" {
		t.Fatal("propagate.yml: no 'Open dogfood-bump PR' step found - workflow structure changed; update this test")
	}
	prIf := ""
	for _, line := range strings.Split(strings.TrimPrefix(prStep, "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "if:") {
			prIf = trimmed
			break
		}
	}
	if prIf == "" {
		t.Errorf("propagate.yml: 'Open dogfood-bump PR' step has no if: directive.\n" +
			"It must gate on steps.auth.outputs.mode == 'pat' so the workflow-file " +
			"push is only attempted with a PAT that has workflow scope.")
	} else if !strings.Contains(prIf, "mode == 'pat'") {
		t.Errorf("propagate.yml: 'Open dogfood-bump PR' if: line is `%s` but does not gate on mode == 'pat'.\n"+
			"Without that gate, the push runs with GITHUB_TOKEN (which is policy-restricted "+
			"from pushing .github/workflows/* changes) and fails. if: line was:\n%s", prIf, prIf)
	}

	// The Skip dogfood-bump PR (no PAT) step must exist as the complement.
	skipStep := stepBlock(body, "Skip dogfood-bump PR (no PAT)")
	if skipStep == "" {
		t.Error("propagate.yml: no 'Skip dogfood-bump PR (no PAT)' step found.\n" +
			"This is the graceful-degradation branch: when AI_WORKFLOWS_PAT is not set, " +
			"the push is skipped and this step emits a ::warning:: with the remediation.")
	}
}
<<<<<<< HEAD


// TestPrReviewSkipsAutomationBots locks the caller-side skip filter for
// automation-bot PR authors in pr-review.yml's review job.
//
// The opencode CLI's assertPermissions() runs a collaborator-permission
// check that returns 'none' for bot accounts (they are not collaborators),
// causing every bot-authored PR to fail the workflow before the AI can
// run. Filtering bot authors at the reusable-workflow level means every
// consumer gets the fix automatically without per-caller config.
//
// If the opencode CLI ever adds a skip path for use_github_token: true
// (the legacy github/index.ts wrapper has one; the current CLI handler
// does not), this filter can be relaxed — but the test stays as a
// regression guard against accidentally removing the filter and breaking
// bot-authored PRs across the consumer fleet again.
func TestPrReviewSkipsAutomationBots(t *testing.T) {
	root := invRoot(t)
	body := readWorkflowFile(t, root, "pr-review.yml")
	reviewJob := stepBlock(body, "Checkout consumer repository")
	if reviewJob == "" {
		t.Fatalf("pr-review.yml: 'Checkout consumer repository' step not found — workflow structure changed; update this test")
	}
	// The `if:` filter lives on the job, just above the steps. stepBlock
	// captures from the `- name:` line through the next blank-line-double-
	// newline, so we look at the whole file for the contains() guard.
	requiredBots := []string{
		`"renovate[bot]"`,
		`"github-actions[bot]"`,
		`"release-please[bot]"`,
		`"dependabot[bot]"`,
	}
	for _, bot := range requiredBots {
		if !strings.Contains(body, bot) {
			t.Errorf("pr-review.yml: missing bot filter for %s.\n"+
				"The opencode CLI rejects bot-authored PRs with `permission: none` (bots are not collaborators). "+
				"Add %s to the job-level `if:` filter on the `review` job so the workflow skips it cleanly instead of failing.", bot, bot)
		}
	}
	if !strings.Contains(body, "contains(fromJson(") {
		t.Errorf("pr-review.yml: expected `contains(fromJson([...]), github.event.pull_request.user.login)` filter on the review job — not found.")
	}
}
=======
>>>>>>> parent of 3c70a28 (fix(workflows): bypass opencode v1.2.9 permission check via TOKEN env (#28))
