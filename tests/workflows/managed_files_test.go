// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// managed_files_test.go guards the TEXT INTEGRITY of the repo's managed
// files: dogfood pin sites (consumers/*.yaml, .github/workflows/self-*.yml)
// and rendered prompts (.github/prompts/*.md).
//
// Motivation (real incident): a sandbox filesystem fault zero-filled five
// managed files (0x00 for every byte, length preserved) and the corrupted
// blobs were committed and pushed as e9523cb on the v0.2.20 release branch.
// Every existing guard greps for tokens or structure, so an all-NUL file
// failed only via indirect, confusing symptoms ("no version tags found",
// "no /command tokens found") several steps after the corruption — and
// nothing guarded the prompt blobs at all. This test fails loudly, naming
// the corrupted file, at the source.
//
// Run: go test ./tests/workflows/...
package workflows

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"unicode/utf8"
)

// managedFileSets returns the managed trees this test guards.
func managedFileSets(t *testing.T) map[string][]string {
	t.Helper()
	root := workflowRoot(t)
	sets := map[string][]string{}

	glob := func(pattern string) []string {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		if len(matches) == 0 {
			t.Fatalf("no files matched %s — the managed layout changed; update this test", pattern)
		}
		return matches
	}

	sets["consumer configs"] = glob("consumers/*.yaml")
	sets["dogfood callers"] = glob(".github/workflows/self-*.yml")
	sets["rendered prompts"] = glob(".github/prompts/*.md")
	return sets
}

// TestManagedFilesAreText asserts every managed file is clean UTF-8 text
// with no NUL bytes, and that pin-bearing files (consumer configs + dogfood
// callers) still contain version tags. The zero-fill incident preserved file
// LENGTH, so size-based sanity checks passed; only content checks catch it.
func TestManagedFilesAreText(t *testing.T) {
	tagRE := regexp.MustCompile(`v[0-9]+\.[0-9]+\.[0-9]+`)
	for set, files := range managedFileSets(t) {
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if bytes.IndexByte(b, 0x00) >= 0 {
				t.Errorf("%s (in %s) contains NUL bytes — zero-filled/binary corruption (see the e9523cb incident); restore it from a good ref before committing.", f, set)
				continue
			}
			if !utf8.Valid(b) {
				t.Errorf("%s (in %s) is not valid UTF-8 — binary corruption; restore it from a good ref before committing.", f, set)
			}
			// Pin-bearing sets must still yield version tags (an empty grep
			// here is what the incident's all-NUL yaml actually produced).
			if set == "consumer configs" || set == "dogfood callers" {
				if len(tagRE.FindAll(b, -1)) == 0 {
					t.Errorf("%s (in %s) contains no vX.Y.Z version tags — a pin site without pins is corrupted or malformed.", f, set)
				}
			}
		}
	}
}
