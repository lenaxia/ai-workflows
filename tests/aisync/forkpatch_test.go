// Package aisync — forked-prompt contract patching tests (review-gate v1).
//
// patch-fork exists because of the LLMSafeSpaces incident (PR #2033): the
// workflow upgrade shipped a verdict validator that REQUIRES a REVIEW-LEDGER
// v1 block, while forked consumers' .github/prompts/pr-review.md — which
// propagate never touches — still taught the old format. Every review in the
// repo went red. These tests pin the patcher's contract: fork content is
// preserved byte-identical, exactly one managed region exists after any
// sequence of patches, stale regions collapse into the fresh one, and the
// patch is loud when the template itself is unmanageable.
//
// Tested through the compiled binary (the ai-sync functions live in package
// main and are not importable), matching sync_test.go's conventions.
package aisync

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forkPatchRepo builds a minimal repo root whose pr-review.md template carries
// the marker-managed contract region (same shape as the real template).
func forkPatchRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tmplDir := filepath.Join(root, "templates", "prompts")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmpl := "You are a code reviewer for the {{ .project_name }} repository.\n\n" +
		"Fork intro line stays.\n\n" +
		"<!-- ai-workflows:review-contract v1 -->\n" +
		"## Your job\n\nYour job is code findings for {{ .project_name }}.\n\n" +
		"REVIEW-LEDGER v1\n" +
		"finding: id=x; class=correctness; anchor=issue:1; pointer=a.go:1; status=new; risk=r; fix_cost=f\n" +
		"<!-- /ai-workflows:review-contract -->\n"
	if err := os.WriteFile(filepath.Join(tmplDir, "pr-review.md"), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "consumers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "consumers", "forkrepo.yaml"), []byte("name: forkrepo\nversion: v0.1.0\nvars:\n  project_name: ForkRepo\nforked:\n  - pr-review.md\nblocks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFork(t *testing.T, root, content string) string {
	t.Helper()
	p := filepath.Join(root, "consumer-out", "pr-review.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func countContractMarkers(s string) int {
	return strings.Count(s, "ai-workflows:review-contract")
}

func patchForkCLI(t *testing.T, bin, root, forkPath string) (string, string, int) {
	t.Helper()
	return runSync(t, bin, root, "patch-fork", "--consumer", "forkrepo", "--file", forkPath)
}

func TestPatchFork_AppendsToFreshFork(t *testing.T) {
	root := forkPatchRepo(t)
	bin := buildBinary(t)
	fork := writeFork(t, root, "You are a code reviewer for ForkRepo.\n\nCUSTOM PROJECT POLICY: check pkg/redact.\n")

	stdout, stderr, code := patchForkCLI(t, bin, root, fork)
	if code != 0 {
		t.Fatalf("patch-fork exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	out, err := os.ReadFile(fork)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	if !strings.HasPrefix(s, "You are a code reviewer for ForkRepo.\n\nCUSTOM PROJECT POLICY: check pkg/redact.") {
		t.Errorf("fork's own content must survive byte-identical at the top, got:\n%.120s", s)
	}
	if !strings.Contains(s, "REVIEW-LEDGER v1") {
		t.Error("appended fork must contain the ledger contract")
	}
	if !strings.Contains(s, "code findings for ForkRepo") {
		t.Error("contract block must be rendered with the consumer's vars")
	}
	if got := countContractMarkers(s); got != 2 { // one START + one END, both carry the stable substring
		t.Errorf("want exactly one contract region (2 marker lines), got %d markers", got)
	}
}

func TestPatchFork_ReplacesStaleBlockKeepsCustomContent(t *testing.T) {
	root := forkPatchRepo(t)
	bin := buildBinary(t)
	stale := "You are a code reviewer for ForkRepo.\n\n" +
		"CUSTOM POLICY A.\n\n" +
		"<!-- ai-workflows:review-contract v0 -->\n" +
		"OLD CONTRACT — ten-section essay mandate.\n" +
		"<!-- /ai-workflows:review-contract -->\n\n" +
		"CUSTOM POLICY B (trailing project content).\n"
	fork := writeFork(t, root, stale)

	stdout, stderr, code := patchForkCLI(t, bin, root, fork)
	if code != 0 {
		t.Fatalf("patch-fork exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	out, _ := os.ReadFile(fork)
	s := string(out)

	if strings.Contains(s, "OLD CONTRACT") || strings.Contains(s, "ten-section essay") {
		t.Error("stale contract content must be fully removed")
	}
	if !strings.Contains(s, "CUSTOM POLICY A.") || !strings.Contains(s, "CUSTOM POLICY B (trailing project content).") {
		t.Error("fork content around the managed region must survive")
	}
	if got := countContractMarkers(s); got != 2 {
		t.Errorf("want exactly one contract region after replace, got %d markers", got)
	}
	if !strings.Contains(s, "REVIEW-LEDGER v1") {
		t.Error("replaced region must be the fresh block")
	}
}

func TestPatchFork_CollapsesMultipleStaleRegions(t *testing.T) {
	root := forkPatchRepo(t)
	bin := buildBinary(t)
	multi := "Intro.\n\n" +
		"<!-- ai-workflows:review-contract v0 -->\nOLD A.\n<!-- /ai-workflows:review-contract -->\n\n" +
		"Middle content.\n\n" +
		"<!-- ai-workflows:review-contract v0 -->\nOLD B.\n<!-- /ai-workflows:review-contract -->\n"
	fork := writeFork(t, root, multi)

	stdout, stderr, code := patchForkCLI(t, bin, root, fork)
	if code != 0 {
		t.Fatalf("patch-fork exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	out, _ := os.ReadFile(fork)
	s := string(out)

	if strings.Contains(s, "OLD A.") || strings.Contains(s, "OLD B.") {
		t.Error("every stale region must be removed, not just the first")
	}
	if got := countContractMarkers(s); got != 2 {
		t.Errorf("want exactly one contract region after collapse, got %d markers", got)
	}
	for _, want := range []string{"Intro.", "Middle content.", "REVIEW-LEDGER v1"} {
		if !strings.Contains(s, want) {
			t.Errorf("patched fork must still contain %q", want)
		}
	}
}

func TestPatchFork_Idempotent(t *testing.T) {
	root := forkPatchRepo(t)
	bin := buildBinary(t)
	fork := writeFork(t, root, "Fork content.\n")

	if _, _, code := patchForkCLI(t, bin, root, fork); code != 0 {
		t.Fatal("patch 1 failed")
	}
	first, _ := os.ReadFile(fork)
	if _, _, code := patchForkCLI(t, bin, root, fork); code != 0 {
		t.Fatal("patch 2 failed")
	}
	second, _ := os.ReadFile(fork)
	if string(first) != string(second) {
		t.Errorf("patch must be idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestPatchFork_UnterminatedStaleRegionDropped(t *testing.T) {
	root := forkPatchRepo(t)
	bin := buildBinary(t)
	// A hand-edit gone wrong: START without END. The patcher must not leave a
	// dangling marker in front of the fresh block.
	broken := "Fork intro.\n\n<!-- ai-workflows:review-contract v0 -->\ndangling stale content\n"
	fork := writeFork(t, root, broken)

	stdout, stderr, code := patchForkCLI(t, bin, root, fork)
	if code != 0 {
		t.Fatalf("patch-fork exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	out, _ := os.ReadFile(fork)
	s := string(out)
	if strings.Contains(s, "dangling stale content") {
		t.Error("unterminated stale region must be dropped to EOF")
	}
	if got := countContractMarkers(s); got != 2 {
		t.Errorf("want exactly one region after repairing an unterminated one, got %d markers", got)
	}
}

func TestPatchFork_MissingFileFails(t *testing.T) {
	root := forkPatchRepo(t)
	bin := buildBinary(t)
	missing := filepath.Join(root, "does", "not", "exist.md")
	_, stderr, code := patchForkCLI(t, bin, root, missing)
	if code == 0 {
		t.Fatal("patching a nonexistent fork file must exit non-zero (the caller's existence check owns the skip decision)")
	}
	if !strings.Contains(stderr, "read forked prompt") {
		t.Errorf("stderr must name the failure site, got: %s", stderr)
	}
}

// TestPatchFork_RealTemplate_EndToEnd runs the patcher against the REAL
// shipped ai-workflows root and asserts the produced fork is shippable:
// parseable markers present and the ledger contract inside.
func TestPatchFork_RealTemplate_EndToEnd(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	realRoot := filepath.Join(filepath.Dir(file), "..", "..")
	bin := buildBinary(t)

	root := forkPatchRepo(t) // disposable fork file lives under the fixture root
	fork := writeFork(t, root, "Fork-specific reviewer policy stays.\n")

	// Real root + real consumer (llmsafespaces), real template — the file is a
	// temp copy, so nothing in the real repo is touched.
	cmd := []string{"patch-fork", "--consumer", "llmsafespaces", "--file", fork, "--repo-root", realRoot}
	stdout, stderr, code := runSyncRaw(t, bin, cmd)
	if code != 0 {
		t.Fatalf("patch-fork against real root exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	out, _ := os.ReadFile(fork)
	s := string(out)
	if !strings.Contains(s, "REVIEW-LEDGER v1") || !strings.Contains(s, "Fork-specific reviewer policy stays.") {
		t.Errorf("real-root patch must carry the ledger contract and keep fork content, got:\n%.200s", s)
	}
	if got := countContractMarkers(s); got != 2 {
		t.Errorf("want exactly one contract region, got %d markers", got)
	}
}

func runSyncRaw(t *testing.T, bin string, args []string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
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

// TestRealReviewTemplateCarriesManagedContractRegion pins the REAL shipped
// template (not the fixture): the marker region must exist and must contain
// the ledger contract — without it every forked consumer silently regresses
// to unverifiable verdicts on the next release.
func TestRealReviewTemplateCarriesManagedContractRegion(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	raw, err := os.ReadFile(filepath.Join(repoRoot, "templates", "prompts", "pr-review.md"))
	if err != nil {
		t.Fatalf("read real template: %v", err)
	}
	s := string(raw)
	startIdx := strings.Index(s, "<!-- ai-workflows:review-contract")
	if startIdx < 0 {
		t.Fatal("real pr-review.md template has no managed contract region — forked consumers cannot receive the verdict contract")
	}
	endIdx := strings.Index(s[startIdx:], "<!-- /ai-workflows:review-contract -->")
	if endIdx < 0 {
		t.Fatal("real pr-review.md template has a contract START marker but no END marker")
	}
	block := s[startIdx : startIdx+endIdx]
	for _, want := range []string{"REVIEW-LEDGER v1", "correctness", "test-integrity", "issue:", "rule:", "claim:"} {
		if !strings.Contains(block, want) {
			t.Errorf("real contract region is missing %q — the fork patch would inject an incomplete contract", want)
		}
	}
}
