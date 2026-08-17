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
// submits and returns the canned comment-list JSON the script reads.
type mockGH struct {
	dir      string
	comments string // raw JSON array the mock returns for /issues/N/comments
	posted   []reviewPayload
}

func newMockGH(t *testing.T, comments string) *mockGH {
	t.Helper()
	dir := t.TempDir()
	// The script calls `gh api .../issues/N/comments --paginate --jq '<filter>'`
	// (fetching the newest review-shaped bot comment) then, on success,
	// `gh api .../pulls/N/reviews -f commit_id= -f event= -f body=`. The mock
	// applies the jq contract itself: for the comments call it emits the
	// canned comment bodies array (the script's jq filters them), and for the
	// reviews POST it reconstructs the -f payload (base64-encoded values so
	// newlines in the body survive the shell).
	// The script calls `gh api .../issues/N/comments --paginate --jq '<filter>'`
	// then `gh api .../pulls/N/reviews -f commit_id= -f event= -f body=`. The
	// mock writes the canned comments to a file; for the comments call it runs
	// real jq with the requested filter over that file (as real gh does), and
	// for the reviews POST it reconstructs the -f payload (base64-encoded
	// values so newlines in the body survive the shell).
	commentsFile := filepath.Join(dir, "comments.json")
	if err := os.WriteFile(commentsFile, []byte(comments), 0o644); err != nil {
		t.Fatalf("write comments fixture: %v", err)
	}
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
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--jq" ]; then
        jqfilter="$arg"
      fi
      prev="$arg"
    done
    if [ -n "$jqfilter" ]; then
      # gh api --jq emits RAW output for string results (like jq -r), not
      # JSON-quoted — emulate that so the script receives an unquoted body.
      jq -r "$jqfilter" %[1]s/comments.json
    else
      cat %[1]s/comments.json
    fi
    ;;
  *) exit 1 ;;
esac
`, dir)
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	m := &mockGH{dir: dir, comments: comments}
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

func TestSalvage_StripsStaleCommitReviewedLine(t *testing.T) {
	dumped := `**Commit reviewed:** 0ldsha123
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
	if len(m.posted) != 1 || contains(m.posted[0].Body, "0ldsha123") {
		t.Errorf("stale Commit reviewed line must be stripped — the API commit_id is authoritative\nposted body:\n%s", m.posted[0].Body)
	}
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

func TestSalvage_PicksNewestReviewShapedComment(t *testing.T) {
	older := comment("## Code Review\n\n### Verdict\n**REQUEST CHANGES** — old.\n")
	newer := comment("## Code Review\n\n### Verdict\n**APPROVE** — new.\n")
	m := newMockGH(t, "["+older+","+newer+"]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertSalvagedTrue(t, out, m, "APPROVED") // newest wins
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

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
