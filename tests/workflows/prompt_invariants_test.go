// prompt_invariants_test.go guards semantic properties of rendered prompt files.
//
// The prompt_coverage_test.go asserts file *existence*; this file asserts
// *content invariants* — properties that must hold after rendering but that
// the renderer itself cannot enforce (they are cross-cutting constraints
// between prompt text and the workflow permissions that consume it).
//
// Run: go test ./tests/workflows/...
package workflows

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildAiSync compiles the ai-sync renderer and returns the binary path.
func buildAiSync(t *testing.T) string {
	t.Helper()
	root := workflowRoot(t)
	bin := filepath.Join(t.TempDir(), "ai-sync")
	cmd := exec.Command("go", "build", "-o", bin, "./scripts/ai-sync")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ai-sync: %v\n%s", err, out)
	}
	return bin
}

// TestIssueResponderIsReadOnly asserts that the rendered issue-responder.md
// for the gokore consumer (which renders, not forks, this file) contains no
// instructions that require git push. The issue-opened workflow runs with
// contents: read + persist-credentials: false, so any push/commit/branch
// instruction will fail at runtime with HTTP 403.
//
// This regression test locks the property introduced after the AI review on
// goKore#334 caught a sync that silently replaced gokore's read-only defense
// with push instructions from the central template.
func TestIssueResponderIsReadOnly(t *testing.T) {
	root := workflowRoot(t)
	bin := buildAiSync(t)
	rendered := t.TempDir()

	cmd := exec.Command(bin, "render", "--repo-root", root,
		"--consumer", "gokore", "--into", rendered)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render gokore: %v\n%s", err, out)
	}

	body, err := os.ReadFile(filepath.Join(rendered, "issue-responder.md"))
	if err != nil {
		t.Fatalf("read rendered issue-responder.md: %v", err)
	}

	denylist := []string{
		"create a feature branch",
		"open a PR",
		"git push",
		"docs/07_WORK_LOG",
		"create a work log",
	}
	for _, phrase := range denylist {
		if strings.Contains(string(body), phrase) {
			t.Errorf("rendered issue-responder.md contains push-triggering phrase %q — "+
				"the issue-opened workflow is read-only (contents: read, "+
				"persist-credentials: false) and cannot perform this action", phrase)
		}
	}
}

// TestRenderedPromptsMatchTemplates asserts that this repo's committed
// rendered prompts (.github/prompts/*.md) are byte-identical to a fresh
// ai-sync render. This catches template-vs-rendered drift: if a template is
// edited but the rendered artifact is not regenerated in the same PR, the
// dogfood workflows (self-pr-review.yml etc.) read stale prompts.
//
// Only checks files this repo renders (not forked). The forked set is defined
// in consumers/ai-workflows.yaml.
func TestRenderedPromptsMatchTemplates(t *testing.T) {
	root := workflowRoot(t)
	bin := buildAiSync(t)
	fresh := t.TempDir()

	cmd := exec.Command(bin, "render", "--repo-root", root,
		"--consumer", "ai-workflows", "--into", fresh)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render ai-workflows: %v\n%s", err, out)
	}

	committedDir := filepath.Join(root, ".github", "prompts")
	entries, err := os.ReadDir(fresh)
	if err != nil {
		t.Fatalf("read rendered dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no files rendered — consumer config or renderer is broken")
	}
	for _, e := range entries {
		freshFile := filepath.Join(fresh, e.Name())
		committedFile := filepath.Join(committedDir, e.Name())
		freshContent, err := os.ReadFile(freshFile)
		if err != nil {
			t.Fatalf("read fresh %s: %v", e.Name(), err)
		}
		committedContent, err := os.ReadFile(committedFile)
		if err != nil {
			t.Errorf("rendered file %s has no committed counterpart at .github/prompts/%s "+
				"(template was rendered but file was not committed)", e.Name(), e.Name())
			continue
		}
		if string(freshContent) != string(committedContent) {
			t.Errorf(".github/prompts/%s is stale — template was edited but rendered file "+
				"was not regenerated. Run: ai-sync render --consumer ai-workflows --into .github/prompts",
				e.Name())
		}
	}
}

// renderAiWorkflowsPrompt renders one prompt for the ai-workflows consumer and
// returns its body (the file is rendered, not forked, for this consumer).
func renderAiWorkflowsPrompt(t *testing.T, name string) string {
	t.Helper()
	root := workflowRoot(t)
	bin := buildAiSync(t)
	rendered := t.TempDir()

	cmd := exec.Command(bin, "render", "--repo-root", root,
		"--consumer", "ai-workflows", "--into", rendered)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render ai-workflows: %v\n%s", err, out)
	}

	body, err := os.ReadFile(filepath.Join(rendered, name))
	if err != nil {
		t.Fatalf("read rendered %s: %v", name, err)
	}
	return string(body)
}

// TestRenovateAnalysisPromptIsReadOnly asserts the rendered renovate-analysis.md
// contains no instructions that require git push / branch creation. The
// renovate-analysis workflow runs with contents: write but the consumer
// checkout is persist-credentials: false, so any push/commit/branch
// instruction fails inside the AI session. Mirror of
// TestIssueResponderIsReadOnly for the renovate-analysis prompt.
func TestRenovateAnalysisPromptIsReadOnly(t *testing.T) {
	body := renderAiWorkflowsPrompt(t, "renovate-analysis.md")

	denylist := []string{
		"create a branch",
		"git push",
		"config/renovate-pr-",
		"gh pr create",
	}
	for _, phrase := range denylist {
		if strings.Contains(body, phrase) {
			t.Errorf("rendered renovate-analysis.md contains push-triggering phrase %q — "+
				"the renovate-analysis workflow's consumer checkout is persist-credentials: false "+
				"and the agent cannot perform this action", phrase)
		}
	}

	// The read-only scoping must be explicit, not merely implied by the
	// absence of push phrases.
	for _, want := range []string{
		"Do NOT create branches or edit files",
		"persist-credentials: false",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered renovate-analysis.md is missing the read-only scoping instruction %q.\n"+
				"The prompt must explicitly tell the agent the checkout is read-only so it never "+
				"attempts branch creation or edits that would fail to push.", want)
		}
	}
}

// TestRenovateAnalysisZeroTargetsDoesNotPostComment asserts the zero-open-PRs
// path does NOT instruct the agent to post a comment. With no open Renovate
// PRs there is no PR to comment on; an untargeted "post a comment" directive
// hands an LLM holding issues: write an improvised destination every two
// hours (the dominant steady state for most repos). The instruction must be
// "report and stop" with no comment at all.
func TestRenovateAnalysisZeroTargetsDoesNotPostComment(t *testing.T) {
	body := renderAiWorkflowsPrompt(t, "renovate-analysis.md")

	for _, phrase := range []string{
		"post one short comment",
		"post a single",
		"post a comment stating",
	} {
		if strings.Contains(body, phrase) {
			t.Errorf("rendered renovate-analysis.md zero-targets path instructs posting a comment (%q) — "+
				"with no open Renovate PRs there is no PR to comment on, and an untargeted write "+
				"lets the model improvise a destination (a new issue, an arbitrary thread) every "+
				"scheduled tick. The instruction must be report-and-stop, no comment.", phrase)
		}
	}

	for _, want := range []string{
		"DO NOT post any comment",
		"no open Renovate PRs were found",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered renovate-analysis.md is missing the report-and-stop zero-targets "+
				"instruction %q. The no-open-PRs path must tell the agent to post nothing and "+
				"report in its final summary.", want)
		}
	}
}

// TestK8sMechanicForksAllTemplates asserts the k8s-mechanic consumer config
// forks EVERY prompt template — the renderer must produce zero files for it.
//
// k8s-mechanic's prompts are 100% repo-specific (controller-runtime
// reconciliation, RemediationJob CRDs, THREAT_MODEL posture — per the config
// header), and the forked list is the only thing keeping propagate from
// clobbering them with the goKore-derived generic templates (the exact
// incident class — silent sync clobber — that motivated
// TestIssueResponderIsReadOnly). The consumer config is parsed by a
// homegrown, schema-less YAML subset (scripts/ai-sync/main.go): a future
// mis-indented or typo'd entry (e.g. `renovate-anaysis.md`) silently lands
// outside the `forked` section and un-forks the file with no test catching
// it — this repo's PR CI is path-filtered and never touches consumers/.
func TestK8sMechanicForksAllTemplates(t *testing.T) {
	root := workflowRoot(t)
	bin := buildAiSync(t)
	rendered := t.TempDir()

	cmd := exec.Command(bin, "render", "--repo-root", root,
		"--consumer", "k8s-mechanic", "--into", rendered)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render k8s-mechanic: %v\n%s", err, out)
	}

	entries, err := os.ReadDir(rendered)
	if err != nil {
		t.Fatalf("read rendered dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	if len(files) > 0 {
		t.Errorf("k8s-mechanic render produced %d file(s): %v — every prompt template must be forked.\n"+
			"The consumer's prompts are repo-specific; any file that renders here will be written by "+
			"propagate and can clobber the consumer's forked copy with the generic template. A "+
			"mis-indented or typo'd entry in the `forked:` list of consumers/k8s-mechanic.yaml "+
			"silently un-forks the file. The config parses with a homegrown YAML subset, so this "+
			"test is the guard. Renderer output:\n%s", len(files), files, out)
	}
}

// TestForkingConsumersDoNotRenderRenovateAnalysis asserts that every consumer
// onboarding to the reusable renovate-analysis workflow (ai-workflows#36)
// forks renovate-analysis.md — the renderer must NOT produce it.
//
// Each consumer runs the reusable workflow from .github/workflows/renovate-
// analysis.yml@v0.2.10, which cats the consumer's OWN forked copy of
// renovate-analysis.md (repo-specific exclusions, project context). If the
// entry is missing from the `forked:` list, the renderer emits the generic
// template and a later propagate run silently clobbers the consumer's forked
// copy with the goKore/template version — the exact silent-sync-clobber
// incident class that motivated TestK8sMechanicForksAllTemplates. The
// homegrown YAML subset parser silently ignores mis-indented entries, so the
// guard must be an explicit per-consumer assertion.
func TestForkingConsumersDoNotRenderRenovateAnalysis(t *testing.T) {
	root := workflowRoot(t)
	bin := buildAiSync(t)

	// Every consumer that has (or is being onboarded to) a forked
	// renovate-analysis.md. Keep in sync with the `forked:` lists in
	// consumers/*.yaml — adding a consumer here without adding the file to
	// its forked list fails the test with the rendered file listed.
	consumers := []string{
		"containers",
		"rathena-client",
		"ai-or-not",
		"gokore",
		"synology-to-immich",
		"ha-custom-components",
		"llmsafespaces",
		"tinyrsvp",
		"talos-ops-prod",
		"k8s-mechanic",
	}

	for _, consumer := range consumers {
		consumer := consumer
		t.Run(consumer, func(t *testing.T) {
			rendered := t.TempDir()
			cmd := exec.Command(bin, "render", "--repo-root", root,
				"--consumer", consumer, "--into", rendered)
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("render %s: %v\n%s", consumer, err, out)
			}

			if _, err := os.Stat(filepath.Join(rendered, "renovate-analysis.md")); err == nil {
				t.Errorf("render for consumer %s produced renovate-analysis.md — the consumer "+
					"forks this prompt (repo-specific exclusions), so it must be in the `forked:` "+
					"list of consumers/%s.yaml. A rendered copy would be clobbered by propagate "+
					"and loses the consumer's repo-specific exclusions.", consumer, consumer)
			}
		})
	}
}
