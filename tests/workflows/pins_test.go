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

// refRE matches `actions/<name>@<hex>` refs. Pin-by-SHA is the repo convention:
// all action refs in .github/workflows/ must be exact SHA-1 pins.
var refRE = regexp.MustCompile(`actions/([a-z0-9._-]+)@([0-9a-f]{40,})`)

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

// workflowFiles returns the paths of all *.yml workflow files.
func workflowFiles(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	if len(files) == 0 {
		t.Fatal("no *.yml workflow files found")
	}
	return files
}

// collect returns every `actions/<name>@<hex>` ref as (name, ref, file).
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
			t.Errorf("%s: actions/%s@%s is %d hex chars, want exactly 40", file, name, pin, len(pin))
		}
	}
}

func TestActionPinsAreConsistentAcrossFiles(t *testing.T) {
	// name -> canonical pin (first occurrence)
	canonical := map[string]string{}
	for _, ref := range collect(t, repoRoot(t)) {
		name, pin, file := ref[0], ref[1], ref[2]
		if prev, ok := canonical[name]; ok && prev != pin {
			t.Errorf("actions/%s pinned inconsistently: %s (from %s) vs %s", name, prev, file, pin)
			continue
		}
		canonical[name] = pin
	}
}
