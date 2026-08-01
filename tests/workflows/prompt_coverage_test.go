// Package workflows guards the GitHub Actions workflow files in .github/workflows/.
//
// This file enforces the caller/prompts contract: every slash command gated in
// .github/workflows/self-ai-comment.yml must have the prompt file(s) that
// route-command.sh reads for it present in .github/prompts/. Under the
// reusable ai-comment.yml's `set -euo pipefail`, route-command.sh does an
// unconditional `cat` of each per-command prompt, so a missing file turns a
// /command comment into a red run with no AI response. See the comment header
// on self-ai-comment.yml and consumers/ai-workflows.yaml for the rationale.
//
// Run: go test ./tests/workflows/...
package workflows

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// workflowRoot resolves the repository root relative to this test file.
// Named distinctly from pins_test.go's repoRoot (a parallel PR) to avoid a
// duplicate-symbol collision once both merge.
func workflowRoot(t *testing.T) string {
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

// commandRE matches the `/command` tokens gated in a workflow's `if:` block.
// Tokens appear as `'/review'` (followed by a quote) or `' /review'`.
var commandRE = regexp.MustCompile(`/(review|ai|fix|implement|test|security|analyze|explain|triage|design|merge|help)(?:['"]|[[:space:]])`)

// promptFilesFor mirrors route-command.sh's prompt assembly for each command:
// the command's own prompt file, plus code-change-workflow.md for the code-
// change commands, plus issue-responder.md for bare /ai on an issue. Keep this
// in sync with scripts/route-command.sh.
func promptFilesFor(cmd string) []string {
	switch cmd {
	case "review":
		return []string{"pr-review.md"}
	case "ai":
		return []string{"pr-review.md", "issue-responder.md"}
	case "fix", "implement", "test", "security", "design":
		return []string{cmd + ".md", "code-change-workflow.md"}
	default: // analyze, explain, triage, merge, help
		return []string{cmd + ".md"}
	}
}

// gatedCommands extracts the /command tokens from the `if:` block of a
// workflow file's content, ignoring `#` comment lines so header prose that
// mentions a command (e.g. "gate /fix once fix.md ships") cannot perturb the
// asserted set.
func gatedCommands(content []byte) []string {
	var ifBlock []byte
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "if:") {
			// Include the `if:` line and the following indented block.
			ifBlock = append(ifBlock, []byte(line)...)
			for _, rest := range lines[i+1:] {
				if strings.TrimSpace(rest) == "" {
					continue
				}
				if len(rest) > 0 && rest[0] != ' ' {
					break
				}
				if strings.HasPrefix(strings.TrimSpace(rest), "#") {
					continue
				}
				ifBlock = append(ifBlock, '\n')
				ifBlock = append(ifBlock, []byte(rest)...)
			}
			break
		}
	}

	seen := map[string]bool{}
	var gated []string
	for _, m := range commandRE.FindAllStringSubmatch(string(ifBlock), -1) {
		cmd := m[1]
		if seen[cmd] {
			continue
		}
		seen[cmd] = true
		gated = append(gated, cmd)
	}
	return gated
}

// TestGatedCommandsHavePromptFiles parses the gated /command tokens out of
// self-ai-comment.yml's if: block and asserts the prompt file(s) each command
// reads exist in .github/prompts/. A broadened filter must ship the prompt
// files in the same PR, or this test fails.
func TestGatedCommandsHavePromptFiles(t *testing.T) {
	root := workflowRoot(t)
	wfPath := filepath.Join(root, ".github", "workflows", "self-ai-comment.yml")
	wfContent, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("read %s: %v", wfPath, err)
	}

	gated := gatedCommands(wfContent)
	if len(gated) == 0 {
		t.Fatalf("no /command tokens found in %s if: block", wfPath)
	}

	promptsDir := filepath.Join(root, ".github", "prompts")
	for _, cmd := range gated {
		for _, p := range promptFilesFor(cmd) {
			full := filepath.Join(promptsDir, p)
			if _, err := os.Stat(full); err != nil {
				t.Errorf("self-ai-comment.yml gates /%s but required prompt %s is missing (does the filter need narrowing?)", cmd, full)
			}
		}
	}
}

// TestGatedCommandsCommentImmunity locks the scope of gatedCommands to the if:
// block: a header comment mentioning a command token must not change the
// asserted gated set.
func TestGatedCommandsCommentImmunity(t *testing.T) {
	root := workflowRoot(t)
	content, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "self-ai-comment.yml"))
	if err != nil {
		t.Fatalf("read self-ai-comment.yml: %v", err)
	}

	before := gatedCommands(content)

	injected := append([]byte("# TODO: gate /fix once fix.md ships\n"), content...)
	after := gatedCommands(injected)

	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("comment prose changed the gated set: %v -> %v (if: block scoping is broken)", before, after)
	}
}
