// opencode_delivery_test.go guards the self-hosted opencode CLI delivery.
//
// The reusable workflows no longer consume anomalyco/opencode/github@<sha>:
// the released CLI's live github handler (github.handler.ts) refuses app-bot
// actors unconditionally (issue #56), so the repo builds the pinned upstream
// ref + the TOKEN-mode assertPermissions bypass patch into its own release
// assets and runs them via scripts/opencode-cli/run.sh (issue #59 for the
// job-level skip half). This file locks that delivery: no upstream action
// pin may return, every Run OpenCode site must go through run.sh, the CLI
// release literal must be identical everywhere, the patch must carry the
// bypass semantics, and the pr-review job gate must admit trusted bots.
//
// Run: go test ./tests/workflows/...
package workflows

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reusableWorkflowsWithOpenCode lists the workflow files that run the CLI.
var reusableWorkflowsWithOpenCode = []string{
	"pr-review.yml",
	"ai-comment.yml",
	"issue-opened.yml",
	"renovate-analysis.yml",
}

// ocReleaseRE extracts the pinned CLI release literal from run.sh.
var ocReleaseRE = regexp.MustCompile(`OC_RELEASE="([^"]+)"`)

// runSh returns the committed scripts/opencode-cli/run.sh content.
func runSh(t *testing.T) string {
	t.Helper()
	root := workflowRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "scripts", "opencode-cli", "run.sh"))
	if err != nil {
		t.Fatalf("read scripts/opencode-cli/run.sh: %v — the self-hosted CLI runner is missing; did the delivery revert to the upstream action?", err)
	}
	return string(b)
}

// TestReusableWorkflowsSelfHostOpenCode locks the four reusable workflows to
// the self-hosted runner script: no `uses: anomalyco/opencode` may return
// (that action installs the released CLI whose live handler refuses app-bot
// actors), and every Run OpenCode step must invoke run.sh.
func TestReusableWorkflowsSelfHostOpenCode(t *testing.T) {
	root := workflowRoot(t)
	for _, wf := range reusableWorkflowsWithOpenCode {
		b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", wf))
		if err != nil {
			t.Fatalf("read %s: %v", wf, err)
		}
		body := string(b)
		if strings.Contains(body, "uses: anomalyco/opencode") {
			t.Errorf("%s: pins anomalyco/opencode/github — the released CLI's assertPermissions refuses app-bot actors (#56). Use .ai-workflows/scripts/opencode-cli/run.sh instead.", wf)
		}
		if !strings.Contains(body, "run: .ai-workflows/scripts/opencode-cli/run.sh") {
			t.Errorf("%s: no Run OpenCode site invokes .ai-workflows/scripts/opencode-cli/run.sh — the self-hosted CLI delivery is broken.", wf)
		}
		if !strings.Contains(body, "run: .ai-workflows/scripts/opencode-cli/run.sh\n") {
			t.Errorf("%s: run.sh invocation has trailing content on its line (a leftover `with:` mapping?) — step is malformed.", wf)
		}
	}
}

// TestOpenCodeReleaseLiteralIsSingleValued asserts run.sh pins exactly one
// release literal and the build workflow can publish it.
func TestOpenCodeReleaseLiteralIsSingleValued(t *testing.T) {
	sh := runSh(t)
	m := ocReleaseRE.FindAllStringSubmatch(sh, -1)
	if len(m) != 1 {
		t.Fatalf("scripts/opencode-cli/run.sh: expected exactly one OC_RELEASE literal, found %d", len(m))
	}
	release := m[0][1]
	if release == "" {
		t.Fatal("scripts/opencode-cli/run.sh: OC_RELEASE is empty")
	}

	root := workflowRoot(t)
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "build-opencode-cli.yml"))
	if err != nil {
		t.Fatalf("read build-opencode-cli.yml: %v", err)
	}
	if !strings.Contains(string(b), "v1.19.3") {
		t.Errorf("build-opencode-cli.yml: upstream opencode ref is not pinned to v1.19.3 — the build is not reproducible.")
	}
	if !strings.Contains(string(b), "0001-github.handler-TOKEN-mode-assertPermissions-bypass.patch") {
		t.Errorf("build-opencode-cli.yml: does not apply the TOKEN-mode bypass patch — the built CLI lacks the fix this delivery exists for.")
	}
}

// TestTokenBypassPatchShipsBypassSemantics asserts the committed patch really
// carries the assert bypass (skip when TOKEN is set), not just any diff.
func TestTokenBypassPatchShipsBypassSemantics(t *testing.T) {
	root := workflowRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "scripts", "opencode-cli", "0001-github.handler-TOKEN-mode-assertPermissions-bypass.patch"))
	if err != nil {
		t.Fatalf("read the TOKEN-mode bypass patch: %v", err)
	}
	patch := string(b)
	for _, want := range []string{
		`process.env["TOKEN"]`,
		`skipped (using github token)`,
		"github.handler.ts",
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch is missing %q — it no longer implements the TOKEN-mode assertPermissions bypass.", want)
		}
	}
}

// TestSelfPrReviewTrustedBotsWired asserts the self-dogfood caller actually
// exercises the trusted_app_bots path (reference adoption for #57/#59) —
// without a live consumer the input would be dead code again.
func TestSelfPrReviewTrustedBotsWired(t *testing.T) {
	root := workflowRoot(t)
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "self-pr-review.yml"))
	if err != nil {
		t.Fatalf("read self-pr-review.yml: %v", err)
	}
	body := string(b)
	if !strings.Contains(body, `trusted_app_bots: '["ai-workflows-propagation[bot]"]'`) {
		t.Errorf("self-pr-review.yml: does not pass trusted_app_bots with ai-workflows-propagation[bot] — the reference adoption of the bot-identity path is gone (see #57/#59).")
	}
}
