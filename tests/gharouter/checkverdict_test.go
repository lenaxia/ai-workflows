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
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
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
	out, outs := runCheckVerdictEnv(t, m)
	return out, outs["delivered"]
}

// runCheckVerdictEnv is runCheckVerdict with extra env (e.g. GITHUB_RUN_ID
// for the run_minutes gate) and returns the full GITHUB_OUTPUT key/value
// map so callers can assert secondary outputs like run_minutes.
func runCheckVerdictEnv(t *testing.T, m *mockGH, extraEnv ...string) (string, map[string]string) {
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
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-verdict.sh must exit 0 (gate semantics live in outputs): %v\noutput:\n%s", err, out)
	}
	outs := map[string]string{}
	if b, rerr := os.ReadFile(outFile); rerr == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if i := strings.Index(line, "="); i > 0 {
				outs[line[:i]] = line[i+1:]
			}
		}
	}
	if outs["delivered"] == "" {
		t.Fatalf("no delivered= line in GITHUB_OUTPUT:\n%s\noutput:\n%s", readGHAOutput(t, outFile), out)
	}
	return string(out), outs
}

// writeJobsFixture installs the actions/runs/{id}/jobs response the mock gh
// serves for check-verdict.sh's run_minutes lookup: the first job whose name
// matches "review" carries startedAt.
func writeJobsFixture(t *testing.T, m *mockGH, startedAt string) {
	t.Helper()
	jobs := map[string]any{
		"jobs": []map[string]any{
			{"name": "review", "started_at": startedAt},
		},
	}
	b, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "jobs.json"), b, 0o644); err != nil {
		t.Fatalf("write jobs fixture: %v", err)
	}
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
// ruling point 4) AND the gate-parsed contract (review round 2, F2): both
// verify steps extract the channel via sed from the EXACT line
// `Verdict delivery channel: <channel>` — pin the full format for all three
// channels so a cosmetic reword fails HERE, not as a fleet-wide false-red.
func TestCheckVerdict_EmitsChannelResolutionLogs(t *testing.T) {
	cases := []struct {
		name    string
		pages   []string
		reviews string
		want    string
	}{
		{"review", []string{"[]"}, `[{"user":{"login":"github-actions[bot]"},"state":"APPROVED","commit_id":"` + headSHA + `"}]`, "review"},
		{"comment", []string{"[" + markerComment("APPROVE", headSHA) + "]"}, "[]", "comment"},
		{"false", []string{"[]"}, "[]", "false"},
	}
	reLine := regexp.MustCompile(`(?m)^Verdict delivery channel: (review|comment|false)\b`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newCheckMockGH(t, tc.pages, tc.reviews)
			out, _ := runCheckVerdict(t, m)
			matches := reLine.FindAllStringSubmatch(out, -1)
			if len(matches) == 0 {
				t.Fatalf("output must contain the exact gate-parsed line 'Verdict delivery channel: <channel>' (verify steps sed-parse this prefix), got:\n%s", out)
			}
			for _, mm := range matches {
				if mm[1] != tc.want {
					t.Errorf("channel line must report %q, got %q\noutput:\n%s", tc.want, mm[1], out)
				}
			}
		})
	}
}

// --- run_minutes: the structural-failure gate for the bounded retry (PR #69) --
//
// Basis (LLMSafeSpaces #1763 comment 6309172907): observed retry rescue rate
// 0%; 42% of first-pass failures die in <60s (startup asserts, quota 429
// storms) where a retry is pure waste. check-verdict.sh emits run_minutes so
// the workflow's retry steps can require a first pass that actually lived.

// PIN: with GITHUB_RUN_ID set and the jobs route serving a review job whose
// started_at is ~5 minutes ago, the script emits run_minutes=4..5 (floor
// arithmetic, ±1 for test/runner clock skew) alongside delivered=.
func TestCheckVerdict_EmitsRunMinutes(t *testing.T) {
	m := newCheckMockGH(t, []string{"[]"}, "[]")
	writeJobsFixture(t, m, time.Now().Add(-5*time.Minute).UTC().Format(time.RFC3339))
	out, outs := runCheckVerdictEnv(t, m, "GITHUB_RUN_ID=12345")
	rm, ok := outs["run_minutes"]
	if !ok {
		t.Fatalf("no run_minutes line in GITHUB_OUTPUT (jobs route served a 5m-old review job):\noutput:\n%s", out)
	}
	n := 0
	if _, err := fmt.Sscanf(rm, "%d", &n); err != nil {
		t.Fatalf("run_minutes must be an integer, got %q", rm)
	}
	if n < 4 || n > 5 {
		t.Errorf("run_minutes for a started_at 5m ago must floor to 4..5 (skew tolerance), got %d", n)
	}
}

// PIN: the emission fails OPEN — every lookup-failure variant (jobs route
// error, no review-named job, unparsable started_at) emits NO run_minutes
// line and leaves delivered semantics untouched, so the workflow gate's
// `run_minutes == ”` disjunct lets the retry proceed as before.
func TestCheckVerdict_RunMinutesFailsOpen(t *testing.T) {
	t.Run("jobs route unavailable", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[]"}, "[]") // no jobs.json => mock exits 1
		out, outs := runCheckVerdictEnv(t, m, "GITHUB_RUN_ID=12345")
		if _, ok := outs["run_minutes"]; ok {
			t.Errorf("jobs API failure must NOT emit run_minutes (fail-open), got %q\noutput:\n%s", outs["run_minutes"], out)
		}
		if outs["delivered"] != "false" {
			t.Errorf("delivered must be unchanged by the failed lookup, got %q", outs["delivered"])
		}
	})
	t.Run("no review-named job", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[]"}, "[]")
		if err := os.WriteFile(filepath.Join(m.dir, "jobs.json"),
			[]byte(`{"jobs":[{"name":"test","started_at":"2026-10-21T00:00:00Z"}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		out, outs := runCheckVerdictEnv(t, m, "GITHUB_RUN_ID=12345")
		if _, ok := outs["run_minutes"]; ok {
			t.Errorf("jq null (no matching job) must NOT emit run_minutes, got %q\noutput:\n%s", outs["run_minutes"], out)
		}
	})
	t.Run("unparsable started_at", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[]"}, "[]")
		writeJobsFixture(t, m, "not-a-timestamp")
		out, outs := runCheckVerdictEnv(t, m, "GITHUB_RUN_ID=12345")
		if _, ok := outs["run_minutes"]; ok {
			t.Errorf("unparsable started_at must NOT emit run_minutes, got %q\noutput:\n%s", outs["run_minutes"], out)
		}
	})
}

// PIN: run_minutes measures JOB elapsed time (now - the review job's
// started_at), not the LLM step alone — the documented approximation in
// check-verdict.sh (checkout/preflight/cache-restore are included; the
// gate's premise is a 0% rescue rate, so the residual overlap is accepted).
// A clock-skewed FUTURE started_at yields a NEGATIVE run_minutes — visible
// here on purpose: negative fails the >= 3 gate CLOSED (retry skipped),
// never re-enables it.
func TestCheckVerdict_RunMinutesMeasuresFromJobStart(t *testing.T) {
	t.Run("two minute old job", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[]"}, "[]")
		writeJobsFixture(t, m, time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339))
		out, outs := runCheckVerdictEnv(t, m, "GITHUB_RUN_ID=12345")
		rm, ok := outs["run_minutes"]
		if !ok {
			t.Fatalf("expected run_minutes emission, got none\noutput:\n%s", out)
		}
		n := 0
		if _, err := fmt.Sscanf(rm, "%d", &n); err != nil {
			t.Fatalf("run_minutes must be an integer, got %q", rm)
		}
		if n < 1 || n > 2 {
			t.Errorf("run_minutes for a 2m-old started_at must floor to 1..2 (job-elapsed semantics, ±skew), got %d", n)
		}
	})
	t.Run("future started_at fails closed visibly", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[]"}, "[]")
		writeJobsFixture(t, m, time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
		out, outs := runCheckVerdictEnv(t, m, "GITHUB_RUN_ID=12345")
		rm, ok := outs["run_minutes"]
		if !ok {
			t.Fatalf("future started_at must still emit run_minutes (the fail-closed skew behavior is pinned, not silent)\noutput:\n%s", out)
		}
		n := 0
		if _, err := fmt.Sscanf(rm, "%d", &n); err != nil || n >= 0 {
			t.Errorf("future started_at must emit a NEGATIVE run_minutes (fails the >=3 gate closed), got %q", rm)
		}
	})
}
