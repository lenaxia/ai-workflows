package workflows

import (
	"strings"
	"testing"
)

// TestPropagateBumpsEachCallerFileFromItsOwnPin guards the consumer-matrix
// pin-bump loop in propagate.yml ("Bump tag in consumer workflow files").
//
// v0.2.27 shipped a silent partial bump: the step discovered ONE global
// OLD_TAG (whichever caller file was scanned first — LLMSafeSpaces'
// ai-comment.yml, pinned v0.2.26) and then sed-replaced only that tag across
// every caller file. pr-review.yml was pinned v0.2.25, so the replacement
// missed it — the sync PR (LLMSafeSpaces#2029) merged with pr-review.yml
// still at v0.2.25 while ai-comment.yml moved to v0.2.27, and the consumer
// kept running the OLD review pipeline with NEW prompts.
//
// The contract pinned here: the bump loop discovers each file's pin INSIDE
// the loop (per-file OLD_TAG + per-file sed), so per-file drift is healed
// instead of skipped.
func TestPropagateBumpsEachCallerFileFromItsOwnPin(t *testing.T) {
	body := readWorkflowFile(t, invRoot(t), "propagate.yml")

	// The global-scan shape must be gone: a `[ -n "$OLD_TAG" ] && break`
	// short-circuits the discovery loop and is the exact defect this test
	// exists to keep dead.
	if strings.Contains(body, `&& break`) {
		t.Errorf("propagate.yml: pin discovery short-circuits with '&& break'.\n" +
			"A single global OLD_TAG taken from the first caller file that has " +
			"one misses every file pinned at a different tag (the v0.2.27 " +
			"partial-bump defect: LLMSafeSpaces pr-review.yml stayed at v0.2.25). " +
			"Discover each file's own pin inside the per-file bump loop.")
	}

	bumpStep := stepBlock(body, "Bump tag in consumer workflow files")
	if bumpStep == "" {
		t.Fatal("propagate.yml: no 'Bump tag in consumer workflow files' step found")
	}

	// The discovery grep must live INSIDE the per-file loop — i.e. it must
	// appear after the loop's `for wf` line and before its sed replacements,
	// so every file gets its own OLD_TAG.
	forLine := strings.Index(bumpStep, `for wf in .github/workflows/`)
	if forLine < 0 {
		t.Fatal("propagate.yml: bump step has no per-file 'for wf' loop")
	}
	grepLine := `OLD_TAG="$(grep -oP 'ai-workflows/.github/workflows/[\w-]+\.yml@\K([0-9a-f]{40}|v[0-9]+\.[0-9]+\.[0-9]+)' "$wf" 2>/dev/null | head -1 || true)"`
	idx := strings.Index(bumpStep[forLine:], grepLine)
	if idx < 0 {
		t.Errorf("propagate.yml: the OLD_TAG discovery grep is not inside the per-file bump loop.\n" +
			"Per-file discovery is what heals callers whose pins have drifted apart; " +
			"moving it back out to a single global scan reintroduces the v0.2.27 partial bump.")
	}

	// The per-file sed replacements must follow the in-loop discovery, and
	// bump both the uses: ref and the version: input.
	sedUses := `sed -i "s|@${OLD_TAG}|@${NEW_TAG}|g" "$wf"`
	sedVer := `sed -i "s|version: ${OLD_TAG}|version: ${NEW_TAG}|g" "$wf"`
	for _, want := range []string{sedUses, sedVer} {
		if !strings.Contains(bumpStep, want) {
			t.Errorf("propagate.yml: bump step is missing %q", want)
		}
	}

	// Every caller file known to propagate must be in the loop's file list —
	// a caller added to the fleet but not to this loop drifts forever.
	for _, wf := range []string{
		".github/workflows/ai-comment.yml",
		".github/workflows/issue-opened.yml",
		".github/workflows/pr-review.yml",
		".github/workflows/renovate-analysis.yml",
	} {
		if !strings.Contains(bumpStep, wf) {
			t.Errorf("propagate.yml: bump loop does not cover %s", wf)
		}
	}
}
