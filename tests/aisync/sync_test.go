// Package aisync tests the ai-sync renderer.
//
// These tests exercise the renderer end-to-end: consumer config parsing,
// template rendering, block overrides, forked-file skipping, banner
// prepending, and variable substitution.
package aisync

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupRepo creates a temporary ai-workflows-like repo with a minimal set of
// templates and consumer configs. Returns the repo root.
func setupRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// Templates
	tmplDir := filepath.Join(root, "templates", "prompts")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "merge.md"), []byte(`You are finalizing a PR for the {{ .project_name }} repository.
{{ block "merge_notes" . }}
{{ end }}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "core-rules.md"), []byte(`## Core Rules

### 1. TDD
Shared TDD text.
{{ block "project_rules" . }}
{{ end }}
### Zero Technical Debt
Shared text.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Consumer: testrepo (basic)
	consumerDir := filepath.Join(root, "consumers", "testrepo")
	if err := os.MkdirAll(consumerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "consumers", "testrepo.yaml"), []byte(`
name: testrepo
version: v0.1.0
vars:
  project_name: TestRepo
  rules_doc: README-LLM.md
forked: []
blocks: []
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Consumer: testrepo-with-overrides
	overrideDir := filepath.Join(root, "consumers", "overrides-repo")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overrideDir, "core-rules.md"), []byte(`{{ define "project_rules" }}
### 2. Custom Rule
Consumer-specific rule content.
{{ end }}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "consumers", "overrides-repo.yaml"), []byte(`
name: overrides-repo
version: v0.1.0
vars:
  project_name: OverridesRepo
forked: []
blocks:
  - core-rules
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Consumer: forked-repo (forks merge.md)
	if err := os.WriteFile(filepath.Join(root, "consumers", "forked-repo.yaml"), []byte(`
name: forked-repo
version: v0.1.0
vars:
  project_name: ForkedRepo
forked:
  - merge.md
blocks: []
`), 0o644); err != nil {
		t.Fatal(err)
	}

	return root
}

// buildBinary compiles ai-sync and returns the binary path.
func buildBinary(t *testing.T) string {
	t.Helper()
	// Find the ai-workflows repo root from the test file location.
	_, file, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")

	bin := filepath.Join(t.TempDir(), "ai-sync")
	cmd := exec.Command("go", "build", "-o", bin, "./scripts/ai-sync")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ai-sync: %v\n%s", err, out)
	}
	return bin
}

// runSync executes the ai-sync binary with the given args.
// args[0] is the command (render/diff/list); --repo-root is inserted after it.
func runSync(t *testing.T, bin, root string, args ...string) (string, string, int) {
	t.Helper()
	cmdName := args[0]
	flags := args[1:]
	fullArgs := append([]string{cmdName, "--repo-root", root}, flags...)
	cmd := exec.Command(bin, fullArgs...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run ai-sync: %v", err)
		}
	}
	return stdout.String(), stderr.String(), exitCode
}

func TestListConsumers(t *testing.T) {
	root := setupRepo(t)
	bin := buildBinary(t)

	stdout, _, code := runSync(t, bin, root, "list")
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	consumers := strings.Fields(stdout)
	want := map[string]bool{"testrepo": true, "overrides-repo": true, "forked-repo": true}
	for _, c := range consumers {
		if !want[c] {
			t.Errorf("unexpected consumer: %s", c)
		}
	}
	for w := range want {
		found := false
		for _, c := range consumers {
			if c == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing consumer: %s", w)
		}
	}
}

func TestRender_BasicVariableSubstitution(t *testing.T) {
	root := setupRepo(t)
	bin := buildBinary(t)
	into := t.TempDir()

	_, _, code := runSync(t, bin, root, "render", "--consumer", "testrepo", "--into", into)
	if code != 0 {
		t.Fatalf("render failed")
	}

	mergePath := filepath.Join(into, "merge.md")
	content, err := os.ReadFile(mergePath)
	if err != nil {
		t.Fatalf("read rendered merge.md: %v", err)
	}
	if !strings.Contains(string(content), "TestRepo") {
		t.Errorf("project_name not substituted:\n%s", content)
	}
}

func TestRender_ManagedBannerPrepended(t *testing.T) {
	root := setupRepo(t)
	bin := buildBinary(t)
	into := t.TempDir()

	runSync(t, bin, root, "render", "--consumer", "testrepo", "--into", into)

	content, _ := os.ReadFile(filepath.Join(into, "merge.md"))
	if !strings.HasPrefix(string(content), "<!-- Managed by lenaxia/ai-workflows@v0.1.0") {
		t.Errorf("managed banner missing:\n%s", content)
	}
}

func TestRender_BlockOverride(t *testing.T) {
	root := setupRepo(t)
	bin := buildBinary(t)
	into := t.TempDir()

	_, _, code := runSync(t, bin, root, "render", "--consumer", "overrides-repo", "--into", into)
	if code != 0 {
		t.Fatalf("render failed")
	}

	content, _ := os.ReadFile(filepath.Join(into, "core-rules.md"))
	if !strings.Contains(string(content), "Custom Rule") {
		t.Errorf("project_rules block not filled:\n%s", content)
	}
	if !strings.Contains(string(content), "Shared TDD text") {
		t.Errorf("shared spine missing:\n%s", content)
	}
	if !strings.Contains(string(content), "Shared text") {
		t.Errorf("shared tech debt missing:\n%s", content)
	}
}

func TestRender_ForkedFileSkipped(t *testing.T) {
	root := setupRepo(t)
	bin := buildBinary(t)
	into := t.TempDir()

	_, stderr, code := runSync(t, bin, root, "render", "--consumer", "forked-repo", "--into", into)
	if code != 0 {
		t.Fatalf("render failed")
	}

	// merge.md should NOT exist (it's forked)
	if _, err := os.Stat(filepath.Join(into, "merge.md")); !os.IsNotExist(err) {
		t.Errorf("forked file merge.md was rendered (should be skipped)")
	}
	// core-rules.md should still exist (not forked)
	if _, err := os.Stat(filepath.Join(into, "core-rules.md")); err != nil {
		t.Errorf("non-forked file core-rules.md was not rendered")
	}
	// Warning should mention the fork
	if !strings.Contains(stderr, "merge.md marked forked") {
		t.Errorf("fork warning missing in stderr:\n%s", stderr)
	}
}

func TestRender_EmptyBlockForConsumerWithoutOverride(t *testing.T) {
	root := setupRepo(t)
	bin := buildBinary(t)
	into := t.TempDir()

	runSync(t, bin, root, "render", "--consumer", "testrepo", "--into", into)

	content, _ := os.ReadFile(filepath.Join(into, "core-rules.md"))
	// project_rules block should be empty (no override defined)
	if strings.Contains(string(content), "Custom Rule") {
		t.Errorf("block should be empty for consumer without override:\n%s", content)
	}
}

func TestDiff_ReportsChangedFiles(t *testing.T) {
	root := setupRepo(t)
	bin := buildBinary(t)
	into := t.TempDir()

	// First render into target
	runSync(t, bin, root, "render", "--consumer", "testrepo", "--into", into)

	// Now change a template and re-diff
	tmplPath := filepath.Join(root, "templates", "prompts", "merge.md")
	os.WriteFile(tmplPath, []byte(`CHANGED {{ .project_name }}`+"\n"), 0o644)

	stdout, _, code := runSync(t, bin, root, "diff", "--consumer", "testrepo", "--into", into)
	if code != 0 {
		t.Fatalf("diff failed")
	}
	if !strings.Contains(stdout, "merge.md") {
		t.Errorf("diff should mention merge.md:\n%s", stdout)
	}
}
