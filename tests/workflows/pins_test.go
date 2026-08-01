// Package workflows guards the GitHub Actions workflow files in .github/workflows/.
//
// These files cannot be exercised by the hermetic aisync/gharouter suites, and
// a malformed action pin silently breaks checkout for every consumer (see
// fix/pr-review-persist-credentials: a 41-char actions/checkout SHA passed CI
// and would have failed the first checkout step of every reusable workflow).
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

// refRE matches pinned action refs of the form `<owner>/<repo>[/<path>]@<hex>`,
// covering both first-party (actions/checkout) and third-party
// (anomalyco/opencode/github) SHA pins. The pre-@ token must contain at least
// one '/', which excludes bare tokens and ${{ }} expressions. Pin-by-SHA is
// the repo convention: every action ref in .github/workflows/ must be an exact
// SHA-1 pin.
var refRE = regexp.MustCompile(`\b([a-zA-Z0-9._-]+(?:/[a-zA-Z0-9._-]+)+)@([0-9a-f]{40,})`)

// repoRoot resolves the repository root relative to this test file
// (<repo>/tests/workflows/pins_test.go).
func repoRoot(t *testing.T) string {
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

// workflowFiles returns the paths of all workflow files (*.yml or *.yaml).
func workflowFiles(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found")
	}
	return files
}

// collect returns every pinned action ref as (name, pin, file).
func collect(t *testing.T, root string) [][3]string {
	t.Helper()
	var out [][3]string
	for _, f := range workflowFiles(t, root) {
		content, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range refRE.FindAllStringSubmatch(string(content), -1) {
			out = append(out, [3]string{m[1], m[2], f})
		}
	}
	return out
}

func TestActionPinsAreValidSHA1(t *testing.T) {
	for _, ref := range collect(t, repoRoot(t)) {
		name, pin, file := ref[0], ref[1], ref[2]
		if len(pin) != 40 {
			t.Errorf("%s: %s@%s is %d hex chars, want exactly 40", file, name, pin, len(pin))
		}
	}
}

func TestActionPinsAreConsistentAcrossFiles(t *testing.T) {
	// name -> canonical (name, pin, file) of the first occurrence.
	canonical := map[string][3]string{}
	for _, ref := range collect(t, repoRoot(t)) {
		name, pin, file := ref[0], ref[1], ref[2]
		if prev, ok := canonical[name]; ok && prev[1] != pin {
			t.Errorf("%s: %s@%s diverges from the pin used in %s (%s@%s)",
				file, name, pin, prev[2], prev[0], prev[1])
			continue
		}
		canonical[name] = ref
	}
}

// TestActionPinsNegativeCase locks in the regression guard independently of the
// live workflow files: a hermetic, table-driven check that a malformed pin is
// flagged and well-formed pins pass. This is the committed version of the
// verification the PR body described as ephemeral.
func TestActionPinsNegativeCase(t *testing.T) {
	const canonical = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"

	// shaLen returns the effective pin length the regex would attribute to ref.
	shaLen := func(ref string) int {
		m := refRE.FindStringSubmatch(ref)
		if m == nil {
			return -1
		}
		return len(m[2])
	}

	tests := []struct {
		name string
		ref  string
		want int // expected captured pin length, or -1 for no match
	}{
		{"canonical first-party pin", "uses: actions/checkout@" + canonical, 40},
		{"third-party pin", "uses: anomalyco/opencode/github@0cf0294787322664c6d668fa5ab0a9ce26796f78", 40},
		{"41-char corrupted pin (regression)", "uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5f447ce83dd", 41},
		// Sub-40-char pins don't satisfy the {40,} quantifier, so the scanner
		// never sees them (returns no match). They are out of this test's scope:
		// the guard exists to catch over-long/corrupted pins, which a greedy
		// {40,} + len!=40 check catches; a truncated pin would be a different
		// defect class.
		{"short pin (out of regex scope, no match)", "uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce", -1},
		{"no slash in name (not an action ref)", "uses: something@de0fac2e4500dabe0009e67214ff5f5447ce83dd", -1},
		{"expression, not a pin", `uses: lenaxia/ai-workflows/.github/workflows/ai-comment.yml@v0.2.0`, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shaLen(tc.ref); got != tc.want {
				t.Errorf("shaLen(%q) = %d, want %d", tc.ref, got, tc.want)
			}
		})
	}

	if len(canonical) != 40 {
		t.Fatalf("test fixture canonical pin is %d chars, want 40", len(canonical))
	}
}
