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
	dir     string
	pages   []string // one raw JSON array per "page" of /issues/N/comments
	reviews string   // raw JSON array served for /pulls/N/reviews (GET)
	posted  []reviewPayload
}

// newMockGH creates a mock gh whose /issues/N/comments call serves the given
// comment pages (one per pagination round) and whose /pulls/N/reviews GET
// serves the given reviews array (default: none — no official verdict on
// record). The script uses `--paginate --slurp`, so the mock wraps all pages
// into one outer array before emitting, mirroring real gh's --slurp.
func newMockGH(t *testing.T, pages []string, reviews string) *mockGH {
	t.Helper()
	if len(pages) == 0 {
		pages = []string{"[]"}
	}
	if reviews == "" {
		reviews = "[]"
	}
	dir := t.TempDir()
	// The script calls `gh api .../issues/N/comments --paginate --slurp`
	// (fetching the newest review-shaped bot comment), `gh api
	// .../pulls/N/reviews` (GET — idempotence check), then on success
	// `gh api .../pulls/N/reviews -f commit_id= -f event= -f body=`. The mock
	// emulates gh: --paginate --slurp serves the outer page array on stdout
	// (the script pipes it through external jq -r); the reviews GET returns
	// the canned list; the reviews POST records the -f payload (base64-encoded
	// values so newlines in the body survive the shell).
	for i, p := range pages {
		f := filepath.Join(dir, fmt.Sprintf("page%d.json", i))
		if err := os.WriteFile(f, []byte(p), 0o644); err != nil {
			t.Fatalf("write page %d fixture: %v", i, err)
		}
	}
	revFile := filepath.Join(dir, "reviews.json")
	if err := os.WriteFile(revFile, []byte(reviews), 0o644); err != nil {
		t.Fatalf("write reviews fixture: %v", err)
	}
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"/pulls/"*"/reviews"*)
    # GET (no -f args): return the canned reviews list (applying the
    # requested --jq filter, as real gh does). POST (-f args): record the
    # payload and echo a review object.
    if printf '%%s ' "$@" | grep -q -- '-f'; then
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
    else
      jqfilter=""
      prev=""
      for arg in "$@"; do
        if [ "$prev" = "--jq" ]; then
          jqfilter="$arg"
        fi
        prev="$arg"
      done
      if [ -n "$jqfilter" ]; then
        jq -r "$jqfilter" %[1]s/reviews.json
      else
        cat %[1]s/reviews.json
      fi
    fi
    ;;
  *"/issues/"*"/comments"*)
    jqfilter=""
    slurp=0
    for arg in "$@"; do
      if [ "$arg" = "--jq" ]; then
        jqfilter=1
      fi
      if [ "$arg" = "--slurp" ]; then
        slurp=1
      fi
    done
    # Extract the filter value following --jq if present.
    if [ "$jqfilter" = "1" ]; then
      jqfilter=""
      prev=""
      for arg in "$@"; do
        if [ "$prev" = "--jq" ]; then
          jqfilter="$arg"
        fi
        prev="$arg"
      done
    fi
    if [ "$slurp" = "1" ] && [ -n "$jqfilter" ]; then
      # Real gh 2.97.0 rejects --slurp with --jq:
      #   the --slurp option is not supported with --jq or --template
      # Mirror the usage error so a script that combines them fails here too.
      echo "the --slurp option is not supported with --jq or --template" >&2
      exit 1
    fi
    if [ "$slurp" = "1" ]; then
      # gh --paginate --slurp: stdout is the outer array of all pages; the
      # script pipes it through external jq -r itself. Concatenate the page
      # arrays (each is already JSON) inside one outer pair.
      echo '['
      first=1
      for f in %[1]s/page*.json; do
        [ "$first" -eq 1 ] || printf ','
        cat "$f"
        first=0
      done
      echo ']'
    else
      # Single page (no slurp): jq runs over the first page.
      jq -r "$jqfilter" %[1]s/page0.json
    fi
    ;;
  *) exit 1 ;;
esac
`, dir)
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	m := &mockGH{dir: dir, pages: pages, reviews: reviews}
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

// path returns the mock gh executable path (for driving it directly).
func (m *mockGH) path() string {
	return filepath.Join(m.dir, "gh")
}

// runSalvage runs the script with the mock gh on PATH and returns its output
// plus the salvaged value written to GITHUB_OUTPUT ("" if none was written).
func runSalvage(t *testing.T, m *mockGH) (string, error) {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "github_output")
	cmd := exec.Command(salvageScriptPath(t))
	cmd.Env = append(os.Environ(),
		"PATH="+m.dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REPOSITORY=lenaxia/test",
		"PR_NUMBER=123",
		"PR_HEAD_SHA=abcdef0123456789",
		"GH_TOKEN=faketoken",
		"GITHUB_OUTPUT="+outFile,
	)
	out, err := cmd.CombinedOutput()
	return string(out) + "\n" + readOutputFile(t, outFile), err
}

// readOutputFile reads the GITHUB_OUTPUT file the script appended to.
func readOutputFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// salvagedValue extracts the LAST salvaged=... line from a GITHUB_OUTPUT
// dump. The script writes salvaged=false up-front and flips to true only on a
// successful POST, so the final value is the contract the workflow gates on.
func salvagedValue(t *testing.T, out string) string {
	t.Helper()
	last := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "salvaged=") {
			last = strings.TrimPrefix(line, "salvaged=")
		}
	}
	if last == "" {
		t.Fatalf("no salvaged= line in output:\n%s", out)
	}
	return last
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
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, "")
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
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, "")
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
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, "")
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
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, "")
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
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, "")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertSalvagedTrue(t, out, m, "CHANGES_REQUESTED")
}

func TestSalvage_NoReviewShapedComment(t *testing.T) {
	chatter := "All verification complete. Composing the final review."
	m := newMockGH(t, []string{"[" + comment(chatter) + "]"}, "")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertNothingPosted(t, m)
}

func TestSalvage_NoCommentsAtAll(t *testing.T) {
	m := newMockGH(t, []string{"[]"}, "")
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
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, "")
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
	m := newMockGH(t, []string{"[" + page1 + "]", "[" + page2 + "]"}, "")
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
	m := newMockGH(t, []string{"[" + human + "]"}, "")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertNothingPosted(t, m)
}

// TestSalvage_MockRejectsSlurpWithJq mirrors real gh 2.97.0's CLI contract:
// `--slurp` combined with `--jq` is a hard usage error. The script must NOT
// combine them (it pipes slurped pages through external jq instead), and
// this mock-level rejection makes the fictional-gh regression impossible to
// reintroduce silently.
func TestSalvage_MockRejectsSlurpWithJq(t *testing.T) {
	// The mock's comments branch, when asked for --slurp + --jq together,
	// must fail exactly like real gh. Drive it directly: run the mock gh as a
	// subprocess with those flags and assert the usage error + non-zero exit.
	m := newMockGH(t, []string{"[]"}, "")
	cmd := exec.Command(m.path())
	cmd.Args = []string{"gh", "api", "repos/lenaxia/test/issues/123/comments", "--paginate", "--slurp", "--jq", ".[]"}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("mock gh must reject --slurp + --jq like real gh 2.97.0; output:\n%s", out)
	}
	if !strings.Contains(string(out), "not supported with --jq") {
		t.Fatalf("mock gh usage error must match real gh; got:\n%s", out)
	}
}

// TestSalvage_FetchFailureEmitsSalvagedFalse asserts the up-front
// salvaged=false write (R2): when the comments fetch fails (mock gh exits 1),
// the script leaves salvaged=false in GITHUB_OUTPUT and posts nothing, so the
// workflow's retry step (gated on salvaged != 'true') proceeds.
func TestSalvage_FetchFailureEmitsSalvagedFalse(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte("#!/usr/bin/env bash\necho 'boom' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(salvageScriptPath(t))
	outFile := filepath.Join(dir, "github_output")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REPOSITORY=lenaxia/test",
		"PR_NUMBER=123",
		"PR_HEAD_SHA=abcdef0123456789",
		"GH_TOKEN=faketoken",
		"GITHUB_OUTPUT="+outFile,
	)
	out, _ := cmd.CombinedOutput()
	if got := salvagedValue(t, string(out)+"\n"+readOutputFile(t, outFile)); got != "false" {
		t.Errorf("script must leave salvaged=false after a fetch failure, got %q", got)
	}
}

// TestSalvage_EmitsSalvagedFalseOnRefusal extends the GITHUB_OUTPUT contract
// to the stale-SHA refusal path: salvaged=false must be present so the retry
// proceeds.
func TestSalvage_EmitsSalvagedFalseOnRefusal(t *testing.T) {
	dumped := "**Commit reviewed:** `0ldsha1234567890`\n## Code Review\n\n### Verdict\n**APPROVE** — old.\n"
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, "")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	if got := salvagedValue(t, out); got != "false" {
		t.Errorf("stale-SHA refusal must leave salvaged=false (retry proceeds), got %q", got)
	}
	assertNothingPosted(t, m)
}

// TestSalvage_EmitsSalvagedTrueOnSuccess extends the GITHUB_OUTPUT contract
// to the success path: salvaged=true after a posted review.
func TestSalvage_EmitsSalvagedTrueOnSuccess(t *testing.T) {
	dumped := "## Code Review\n\n### Verdict\n**APPROVE** — ok.\n"
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, "")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	if got := salvagedValue(t, out); got != "true" {
		t.Errorf("successful salvage must set salvaged=true, got %q", got)
	}
	assertSalvagedTrue(t, out, m, "APPROVED")
}

// TestSalvage_SkipsWhenOfficialVerdictExists is the R4 regression: after a
// successful retry delivers an official verdict for the head SHA, a salvage
// pass must NOT re-post a (pre-retry) dumped comment — that would make the
// stale attempt the PR's newest official review, potentially overriding the
// retry's fresh verdict (including stale-APPROVE over fresh-CHANGES_REQUESTED).
// The script's idempotence check (GET /pulls/N/reviews for an existing bot
// APPROVED/CHANGES_REQUESTED on the head) must short-circuit before the POST.
func TestSalvage_SkipsWhenOfficialVerdictExists(t *testing.T) {
	dumped := "## Code Review\n\n### Verdict\n**APPROVE** — stale pre-retry dump.\n"
	// An existing official CHANGES_REQUESTED for the head SHA — the retry's
	// fresh, authoritative verdict. The stale APPROVE dump must NOT override it.
	existing := `[{"user":{"login":"github-actions[bot]"},"state":"CHANGES_REQUESTED","commit_id":"abcdef0123456789"}]`
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, existing)
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	if got := salvagedValue(t, out); got != "false" {
		t.Errorf("script must skip salvage when an official head-SHA verdict exists (salvaged=false), got %q\noutput:\n%s", got, out)
	}
	assertNothingPosted(t, m)
}

// TestSalvage_PostsWhenNoOfficialVerdict is the positive control for the
// idempotence check: with NO existing official review, a SHA-matching dump is
// still salvaged.
func TestSalvage_PostsWhenNoOfficialVerdict(t *testing.T) {
	dumped := "## Code Review\n\n### Verdict\n**APPROVE** — ok.\n"
	m := newMockGH(t, []string{"[" + comment(dumped) + "]"}, "[]")
	out, err := runSalvage(t, m)
	if err != nil {
		t.Fatalf("script must exit 0: %v\noutput:\n%s", err, out)
	}
	assertSalvagedTrue(t, out, m, "APPROVED")
}
