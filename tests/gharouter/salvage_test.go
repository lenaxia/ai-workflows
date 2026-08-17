// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gharouter also exercises scripts/salvage-verdict.sh, the recovery
// path that re-posts a review verdict that opencode dumped as a plain issue
// comment into an official review.
//
// The script is bash; these tests invoke it as a subprocess with a mock `gh`
// binary on PATH (the script shells out to `gh api`) and assert on the
// salvaged=true|false output and the review JSON posted to the mock.
package gharouter

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// salvageScriptPath resolves scripts/salvage-verdict.sh relative to the repo.
func salvageScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	p := filepath.Join(repoRoot, "scripts", "salvage-verdict.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("salvage-verdict.sh not found at %s: %v", p, err)
	}
	return p
}

// reviewPayload mirrors the review the script POSTs to /pulls/N/reviews.
// The mock gh base64-encodes values to survive newlines in the body, so
// CommitID/Event/Body are the DECODED forms.
type reviewPayload struct {
	CommitID string `json:"commit_id"`
	Event    string `json:"event"`
	Body     string `json:"body"`
}

// decodePayload base64-decodes the mock's raw JSON into the struct.
func decodePayload(t *testing.T, b []byte) reviewPayload {
	t.Helper()
	var raw struct {
		CommitID string `json:"commit_id"`
		Event    string `json:"event"`
		Body     string `json:"body"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("mock posted.json is invalid JSON: %v\n%s", err, b)
	}
	dec := func(s string) string {
		if s == "" {
			return ""
		}
		d, err := decodeB64(s)
		if err != nil {
			t.Fatalf("mock value not base64: %v", err)
		}
		return string(d)
	}
	return reviewPayload{CommitID: dec(raw.CommitID), Event: dec(raw.Event), Body: dec(raw.Body)}
}

func decodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// mockGH is a fake `gh` executable. It records the review payload the script
// submits and returns canned comment-page JSON the script reads.
type mockGH struct {
	dir    string
	pages  []string // one raw JSON array per "page" of /issues/N/comments
	posted []reviewPayload
}

// newMockGH creates a mock gh whose /issues/N/comments call serves the given
// comment pages (one per pagination round). The script uses
// `--paginate --slurp`, so the mock wraps all pages into one outer array
// before applying the requested jq filter, mirroring real gh's --slurp.
func newMockGH(t *testing.T, pages ...string) *mockGH {
	t.Helper()
	if len(pages) == 0 {
		pages = []string{"[]"}
	}
	dir := t.TempDir()
	// The script calls `gh api .../issues/N/comments --paginate --slurp
	// --jq '<filter>'` (fetching the newest review-shaped bot comment) then,
	// on success, `gh api .../pulls/N/reviews -f commit_id= -f event=
	// -f body=`. The mock emulates gh: each page is served as a separate
	// array, --slurp wraps them into one outer array, and the jq filter runs
	// once over the slurped input (gh --slurp semantics). For the reviews
	// POST it reconstructs the -f payload (base64-encoded values so newlines
	// in the body survive the shell).
	for i, p := range pages {
		f := filepath.Join(dir, fmt.Sprintf("page%d.json", i))
		if err := os.WriteFile(f, []byte(p), 0o644); err != nil {
			t.Fatalf("write page %d fixture: %v", i, err)
		}
	}
	// Join the page arrays into one outer array for the --slurp path. Each
	// page is already a JSON array; concatenating with commas gives the
	// slurped outer array.
	slurped := strings.Join(pages, ",")
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
printf '%%s\x00' "$@" >> %[1]s/args.log
case "$*" in
  *"/pulls/"*"/reviews"*)
    {
      echo "{"
      first=1
      prev="-"
      for arg in "$@"; do
        if [ "$prev" = "-f" ]; then
          key="${arg%%=*}"
          val="${arg#*=}"
          [ "$first" -eq 1 ] || printf ',\n'
          printf '"%%s": "%%s"' "$key" "$(printf '%%s' "$val" | base64 -w0)"
          first=0
        fi
        prev="$arg"
      done
      echo ""
      echo "}"
    } > %[1]s/posted.json
    echo '{"id":1}'
    ;;
  *"/issues/"*"/comments"*)
    jqfilter=""
    slurp=0
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--jq" ]; then
        jqfilter="$arg"
      fi
      if [ "$prev" = "--slurp" ]; then
        slurp=1
      fi
      prev="$arg"
    done
    if [ "$slurp" = "1" ]; then
      # gh --paginate --slurp: jq input is the outer array of all pages.
      printf '%%s' '[%[2]s]' | jq -r "$jqfilter"
    else
      # Single page (no slurp): jq runs over the first page.
      jq -r "$jqfilter" %[1]s/page0.json
    fi
    ;;
  *) exit 1 ;;
esac
`, dir, slurped)
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	m := &mockGH{dir: dir, pages: pages}
	return m
}

// loadPosted reads posted.json (written by the mock gh during a run) into
// m.posted. Call after each runSalvage to inspect what the script submitted.
func (m *mockGH) loadPosted(t *testing.T) {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(m.dir, "posted.json")); err == nil {
		m.posted = append(m.posted, decodePayload(t, b))
	}
}

// runSalvage runs the script with the mock gh on PATH and returns its output.
func runSalvage(t *testing.T, m *mockGH) (string, error) {
	t.Helper()
	cmd := exec.Command(salvageScriptPath(t))
	cmd.Env = append(os.Environ(),
		"PATH="+m.dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REPOSITORY=lenaxia/test",
		"PR_NUMBER=123",
		"PR_HEAD_SHA=abcdef0123456789",
		"GH_TOKEN=faketoken",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// comment returns a JSON element for a bot comment with the given body.
func comment(body string) string {
	b, _ := json.Marshal(map[string]any{"user": map[string]any{"login": "github-actions[bot]"}, "body": body})
	return string(b)
}

// assertSalvagedTrue asserts the script reported salvaged=true and posted one
// review with the expected event.
func assertSalvagedTrue(t *testing.T, out string, m *mockGH, wantEvent string) {
	t.Helper()
	m.loadPosted(t)
	if len(m.posted) != 1 {
		t.Fatalf("expected exactly 1 posted review, got %d\noutput:\n%s", len(m.posted), out)
	}
	p := m.posted[0]
	if p.CommitID != "abcdef0123456789" {
		t.Errorf("review must be pinned to the head SHA, got %q", p.CommitID)
	}
	if p.Event != wantEvent {
		t.Errorf("expected event %s, got %s", wantEvent, p.Event)
	}
}

// assertNothingPosted asserts the script did NOT create an official review.
func assertNothingPosted(t *testing.T, m *mockGH) {
	t.Helper()
	m.loadPosted(t)
	if len(m.posted) != 0 {
		t.Fatalf("must not post a review; posted: %+v", m.posted)
	}
}

func TestSalvage_RecoversDumpedRequestChanges(t *testing.T) {
	dumped := `## Code Review

### Summary
A review.

### Verdict
**REQUEST CHANGES** — two items remain.
`
	m := newMockGH(t, "["+comment(dumped)+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertSalvagedTrue(t, out, m, "CHANGES_REQUESTED")
}

func TestSalvage_RecoversDumpedApprove(t *testing.T) {
	dumped := `## Code Review

### Summary
All good.

### Verdict
**APPROVE** — zero findings.
`
	m := newMockGH(t, "["+comment(dumped)+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertSalvagedTrue(t, out, m, "APPROVED")
}

func TestSalvage_StripsMatchingCommitReviewedLine(t *testing.T) {
	// A comment whose Commit reviewed line MATCHES the head SHA is salvaged,
	// and the line is stripped from the posted body (commit_id is set via the
	// API, so the body line is redundant).
	dumped := `**Commit reviewed:** ` + "`" + `abcdef0123456789` + "`" + `
## Code Review

### Summary
review

### Verdict
**APPROVE** — ok.
`
	m := newMockGH(t, "["+comment(dumped)+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertSalvagedTrue(t, out, m, "APPROVED")
	if len(m.posted) != 1 || strings.Contains(m.posted[0].Body, "Commit reviewed:") {
		t.Errorf("matching Commit reviewed line must be stripped from the posted body\nposted body:\n%s", m.posted[0].Body)
	}
}

func TestSalvage_RefusesStaleCommitReviewedSHA(t *testing.T) {
	// R1: a comment declaring a DIFFERENT reviewed SHA is a stale verdict
	// from an earlier commit. Re-pinning it onto the current head would mark
	// unreviewed commits as approved — the ONLY safe behavior is to refuse
	// and fall through to the LLM retry.
	dumped := "**Commit reviewed:** `0ldsha1234567890`\n## Code Review\n\n### Verdict\n**APPROVE** — old.\n"
	m := newMockGH(t, "["+comment(dumped)+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertNothingPosted(t, m)
}

func TestSalvage_VerdictScopedToVerdictSection(t *testing.T) {
	// C2: an APPROVE mentioned in the Summary must never flip the event when
	// the ### Verdict section says REQUEST CHANGES.
	dumped := `## Code Review

### Summary
Close to **APPROVE** but two blocking findings remain.

### Verdict
**REQUEST CHANGES** — see Correctness.
`
	m := newMockGH(t, "["+comment(dumped)+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertSalvagedTrue(t, out, m, "CHANGES_REQUESTED")
}

func TestSalvage_NoReviewShapedComment(t *testing.T) {
	chatter := "All verification complete. Composing the final review."
	m := newMockGH(t, "["+comment(chatter)+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertNothingPosted(t, m)
}

func TestSalvage_NoCommentsAtAll(t *testing.T) {
	m := newMockGH(t, "[]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertNothingPosted(t, m)
}

func TestSalvage_UnparseableVerdictNotSalvaged(t *testing.T) {
	dumped := `## Code Review

### Summary
review

### Verdict
The changes look fine.
`
	m := newMockGH(t, "["+comment(dumped)+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0 with salvaged=false, not error: %v\noutput:\n%s", err, out)
	}
	assertNothingPosted(t, m)
}

func TestSalvage_PaginatedComments_PicksNewestGlobally(t *testing.T) {
	// C1 regression: --paginate runs the jq filter PER PAGE and concatenates,
	// so `last` on a single page would pick the OLDEST page's newest comment.
	// --slurp must aggregate across pages before selecting. Page 1 (older)
	// newest review-shaped comment is a REQUEST CHANGES dump; page 2 (newer)
	// is an APPROVE dump. Pre-fix: event=CHANGES_REQUESTED from page 1 with a
	// concatenated body. Post-fix: event=APPROVED from page 2 only.
	page1 := comment("## Code Review\n\n### Verdict\n**REQUEST CHANGES** — old page1.\n")
	page2 := comment("## Code Review\n\n### Verdict\n**APPROVE** — new page2.\n")
	m := newMockGH(t, "["+page1+"]", "["+page2+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertSalvagedTrue(t, out, m, "APPROVED") // globally newest wins
	if len(m.posted) != 1 || strings.Contains(m.posted[0].Body, "page1") {
		t.Errorf("salvaged body must be the globally newest page's dump only\nposted body:\n%s", m.posted[0].Body)
	}
}

func TestSalvage_NonBotCommentsIgnored(t *testing.T) {
	human := `{"user": {"login": "lenaxia"}, "body": "## Code Review\n\n### Verdict\n**APPROVE**\n"}`
	m := newMockGH(t, "["+human+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertNothingPosted(t, m)
}
