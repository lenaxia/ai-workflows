// Package workflows guards the GitHub Actions workflow files in .github/workflows/.
//
// This file locks the dogfood pin surface: the `uses:...@<tag>` / `version:`
// tokens in the self-dogfood callers (self-ai-comment.yml, self-pr-review.yml,
// self-renovate-analysis.yml) and the consumer config (consumers/ai-workflows.yaml)
// must all agree. The propagate.yml bump is sed-based and can touch one file
// and not the others, silently re-introducing pin drift (see the #15 review
// and the fix that aligned everything at v0.2.1).
//
// Run: go test ./tests/workflows/...
package workflows

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// tagRE matches a pinned version tag such as @v0.2.1 or version: v0.2.1.
var tagRE = regexp.MustCompile(`v[0-9]+\.[0-9]+\.[0-9]+`)

// dogfoodPinSites are the files whose pins must agree.
var dogfoodPinSites = []string{
	"consumers/ai-workflows.yaml",
	".github/workflows/self-ai-comment.yml",
	".github/workflows/self-pr-review.yml",
	".github/workflows/self-renovate-analysis.yml",
}

// pins returns the set of version tags found in a file.
func pins(t *testing.T, root, rel string) []string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var out []string
	for _, m := range tagRE.FindAllString(string(content), -1) {
		out = append(out, m)
	}
	if len(out) == 0 {
		t.Fatalf("no version tags found in %s", rel)
	}
	return out
}

// TestDogfoodPinsAreConsistent asserts every pin site references the same tag,
// so a partial bump (one file ahead of the others) fails loudly instead of
// silently running the dogfood surface at two different versions.
func TestDogfoodPinsAreConsistent(t *testing.T) {
	root := workflowRoot(t)
	var first string
	for _, site := range dogfoodPinSites {
		for _, tag := range pins(t, root, site) {
			if first == "" {
				first = tag
			} else if tag != first {
				t.Errorf("pin drift: %s references %s but %s sets %s", site, tag, dogfoodPinSites[0], first)
			}
		}
	}
}
