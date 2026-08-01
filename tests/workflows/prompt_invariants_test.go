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
