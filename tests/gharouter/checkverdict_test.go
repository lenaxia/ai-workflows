// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gharouter also exercises scripts/check-verdict.sh, the single
// source of truth for "was a review verdict delivered against this PR's
// head SHA?" used by BOTH gate steps of the reusable pr-review workflow
// (Check verdict delivered / Verify review submitted) and the /review tail
// of ai-comment.yml.
//
// Delivery is two-channel (owner ruling ai-workflows#6209846676, issue #46):
//
//   - channel "review":  an official APPROVED/CHANGES_REQUESTED review by
//     github-actions[bot] pinned to the head SHA (the historical gate), OR
//   - channel "comment": a github-actions[bot] PR comment carrying the
//     machine-greppable marker
//     `<!-- ai-workflows-verdict: APPROVE|REQUEST_CHANGES sha=<full-sha> -->`
//     whose sha equals the head SHA — the REQUIRED fallback when GitHub
//     refuses the review event itself (HTTP 422 "GitHub Actions is not
//     permitted to approve pull requests").
//
// Red failure (delivered=false) is reserved for "neither channel could
// post". The script is bash; these tests invoke it as a subprocess with a
// mock `gh` binary on PATH (mirroring salvage_test.go).
package gharouter

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// checkVerdictScriptPath resolves scripts/check-verdict.sh relative to the repo.
func checkVerdictScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	p := filepath.Join(repoRoot, "scripts", "check-verdict.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("check-verdict.sh not found at %s: %v", p, err)
	}
	return p
}

// newCheckMockGH builds a mock gh serving the given comment pages and
// reviews list for check-verdict.sh's two GETs. Reuses the salvage mock's
// wire behavior (pages via --paginate --slurp; reviews via --jq filter).
func newCheckMockGH(t *testing.T, pages []string, reviews string) *mockGH {
	t.Helper()
	return newMockGH(t, pages, reviews)
}

// runCheckVerdict runs check-verdict.sh with the mock gh on PATH and
// returns combined output plus the delivered= value from GITHUB_OUTPUT.
func runCheckVerdict(t *testing.T, m *mockGH) (string, string) {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "github_output")
	cmd := exec.Command(checkVerdictScriptPath(t))
	cmd.Env = append(os.Environ(),
		"PATH="+m.dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REPOSITORY=lenaxia/test",
		"PR_NUMBER=123",
		"PR_HEAD_SHA=abcdef0123456789abcdef0123456789abcdef01",
		"GH_TOKEN=faketoken",
		"GITHUB_OUTPUT="+outFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-verdict.sh must exit 0 (gate semantics live in outputs): %v\noutput:\n%s", err, out)
	}
	delivered := ""
	if b, rerr := os.ReadFile(outFile); rerr == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "delivered=") {
				delivered = strings.TrimPrefix(line, "delivered=")
			}
		}
	}
	if delivered == "" {
		t.Fatalf("no delivered= line in GITHUB_OUTPUT:\n%s\noutput:\n%s", readGHAOutput(t, outFile), out)
	}
	return string(out), delivered
}

func readGHAOutput(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// markerComment returns a JSON element for a bot comment whose body starts
// with the machine-greppable verdict marker for the given event+sha.
func markerComment(event, sha string) string {
	body := fmt.Sprintf("<!-- ai-workflows-verdict: %s sha=%s -->\n**Commit reviewed:** `%s`\n\n## Code Review\n\n### Summary\nok\n\n### Verdict\n**%s** — zero findings.\n", event, sha, sha, eventLabel(event))
	return rawComment("github-actions[bot]", body)
}

// markerCommentBody is markerComment without the JSON wrapper (for raw-page fixtures).
func markerCommentBody(event, sha string) string {
	body := fmt.Sprintf("<!-- ai-workflows-verdict: %s sha=%s -->\n**Commit reviewed:** `%s`\n\n## Code Review\n\n### Summary\nok\n\n### Verdict\n**%s** — zero findings.\n", event, sha, sha, eventLabel(event))
	return body
}

func eventLabel(event string) string {
	if event == "APPROVE" {
		return "APPROVE"
	}
	return "REQUEST CHANGES"
}

// rawComment returns a JSON element for a comment by login with a raw body.
func rawComment(login, body string) string {
	b, _ := json.Marshal(map[string]any{"user": map[string]any{"login": login}, "body": body})
	return string(b)
}

const headSHA = "abcdef0123456789abcdef0123456789abcdef01"
const otherSHA = "0000000000000000000000000000000000000000"

// PIN 1 (owner ruling 6209846676): when the official review channel is
// platform-refused (422 approve-block, issue #46) and the reviewer posted
// the verdict as a marker comment, the gate MUST count it as delivered.
// This is the first pin of the fallback matrix.
func TestCheckVerdict_ApproveRefusedFallsBackToComment(t *testing.T) {
	m := newCheckMockGH(t, []string{"[" + markerComment("APPROVE", headSHA) + "]"}, "[]")
	out, delivered := runCheckVerdict(t, m)
	if delivered != "comment" {
		t.Errorf("marker comment (APPROVE, matching sha) must deliver via channel 'comment', got %q\noutput:\n%s", delivered, out)
	}
}

func TestCheckVerdict_RequestChangesMarkerAlsoDelivers(t *testing.T) {
	m := newCheckMockGH(t, []string{"[" + markerComment("REQUEST_CHANGES", headSHA) + "]"}, "[]")
	out, delivered := runCheckVerdict(t, m)
	if delivered != "comment" {
		t.Errorf("marker comment (REQUEST_CHANGES, matching sha) must deliver via channel 'comment', got %q\noutput:\n%s", delivered, out)
	}
}

// Official review wins over (or alongside) any comment: the review channel
// is authoritative when it exists.
func TestCheckVerdict_OfficialReviewDelivers(t *testing.T) {
	reviews := `[{"user":{"login":"github-actions[bot]"},"state":"APPROVED","commit_id":"` + headSHA + `"}]`
	m := newCheckMockGH(t, []string{"[]"}, reviews)
	out, delivered := runCheckVerdict(t, m)
	if delivered != "review" {
		t.Errorf("official bot APPROVED on head SHA must deliver via channel 'review', got %q\noutput:\n%s", delivered, out)
	}
}

// Review AND marker comment both present: review channel is reported (the
// comment is redundant, never a downgrade).
func TestCheckVerdict_ReviewWinsOverComment(t *testing.T) {
	reviews := `[{"user":{"login":"github-actions[bot]"},"state":"CHANGES_REQUESTED","commit_id":"` + headSHA + `"}]`
	m := newCheckMockGH(t, []string{"[" + markerComment("APPROVE", headSHA) + "]"}, reviews)
	_, delivered := runCheckVerdict(t, m)
	if delivered != "review" {
		t.Errorf("official review must take precedence over a marker comment, got %q", delivered)
	}
}

// A marker whose sha does NOT equal the head SHA is a stale verdict — it
// must never count (mirrors salvage-verdict.sh's freshness refusal).
func TestCheckVerdict_StaleMarkerSHARefused(t *testing.T) {
	m := newCheckMockGH(t, []string{"[" + markerComment("APPROVE", otherSHA) + "]"}, "[]")
	out, delivered := runCheckVerdict(t, m)
	if delivered != "false" {
		t.Errorf("stale-sha marker comment must NOT deliver, got %q\noutput:\n%s", delivered, out)
	}
}

// A review-shaped comment WITHOUT the marker is the historical "dumped
// verdict" class: the delivery gate does not count it (salvage-verdict.sh
// owns upgrading it to an official review; verify re-runs this script
// fresh afterwards and then sees the review).
func TestCheckVerdict_UnmarkedDumpedCommentNotDelivered(t *testing.T) {
	dumped := "## Code Review\n\n### Verdict\n**APPROVE** — ok.\n"
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", dumped) + "]"}, "[]")
	out, delivered := runCheckVerdict(t, m)
	if delivered != "false" {
		t.Errorf("unmarked dumped comment must NOT satisfy the delivery gate, got %q\noutput:\n%s", delivered, out)
	}
}

// Only bot-authored markers count — a human forging the marker must not.
func TestCheckVerdict_HumanMarkerIgnored(t *testing.T) {
	m := newCheckMockGH(t, []string{"[" + rawComment("lenaxia", markerCommentBody("APPROVE", headSHA)) + "]"}, "[]")
	out, delivered := runCheckVerdict(t, m)
	if delivered != "false" {
		t.Errorf("human-authored marker must NOT deliver, got %q\noutput:\n%s", delivered, out)
	}
}

// Official review pinned to a DIFFERENT commit (stale) does not deliver —
// the historical head-SHA pinning must survive the two-channel rework.
func TestCheckVerdict_StaleOfficialReviewRefused(t *testing.T) {
	reviews := `[{"user":{"login":"github-actions[bot]"},"state":"APPROVED","commit_id":"` + otherSHA + `"}]`
	m := newCheckMockGH(t, []string{"[]"}, reviews)
	out, delivered := runCheckVerdict(t, m)
	if delivered != "false" {
		t.Errorf("stale official review must NOT deliver, got %q\noutput:\n%s", delivered, out)
	}
}

// Neither channel posted anything: delivered=false (the loud-red cell).
func TestCheckVerdict_NothingDelivered(t *testing.T) {
	m := newCheckMockGH(t, []string{"[]"}, "[]")
	out, delivered := runCheckVerdict(t, m)
	if delivered != "false" {
		t.Errorf("empty PR must yield delivered=false, got %q\noutput:\n%s", delivered, out)
	}
}

// Marker comments paginate: a busy PR can push the marker beyond page 1 —
// the comment channel must aggregate across pages (same class as the
// salvage C1 regression).
func TestCheckVerdict_MarkerPaginates(t *testing.T) {
	page1 := "[" + rawComment("github-actions[bot]", "noise") + "]"
	page2 := "[" + markerComment("APPROVE", headSHA) + "]"
	m := newCheckMockGH(t, []string{page1, page2}, "[]")
	out, delivered := runCheckVerdict(t, m)
	if delivered != "comment" {
		t.Errorf("marker on page 2 must still deliver, got %q\noutput:\n%s", delivered, out)
	}
}

// The channel-resolution log lines are the production e2e proof (owner
// ruling point 4): the run log must show BOTH channels being evaluated so
// a delivered verdict demonstrably exercises the OR-comment path.
func TestCheckVerdict_EmitsChannelResolutionLogs(t *testing.T) {
	m := newCheckMockGH(t, []string{"[" + markerComment("APPROVE", headSHA) + "]"}, "[]")
	out, _ := runCheckVerdict(t, m)
	for _, want := range []string{"review", "comment"} {
		if !strings.Contains(out, want) {
			t.Errorf("output must mention channel %q for production proof, got:\n%s", want, out)
		}
	}
}
