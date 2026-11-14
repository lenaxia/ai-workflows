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
// post" AND — review gate v1 (docs/review-gate-v1-spec.md §4) — "a verdict
// was delivered but its body failed validation" (missing/malformed ledger,
// head/round mismatch, approve-with-open-blocking, coverage gap, invalid
// pointer, CI-claim mismatch); the latter emits rejection_reason= so the
// bounded retry prompt receives the machine-readable cause.
//
// The script is bash; these tests invoke it as a subprocess with a mock
// `gh` binary on PATH (mirroring salvage_test.go).
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
// map so callers can assert secondary outputs like run_minutes and
// rejection_reason.
func runCheckVerdictEnv(t *testing.T, m *mockGH, extraEnv ...string) (string, map[string]string) {
	t.Helper()
	return runCheckVerdictEnvDir(t, m, "", extraEnv...)
}

// runCheckVerdictEnvDir is runCheckVerdictEnv with the script's working
// directory overridden (the pointer spot-check verifies finding pointers
// against the checkout — a temp fixture dir stands in for the consumer
// worktree; empty dir = inherit the test process's CWD).
func runCheckVerdictEnvDir(t *testing.T, m *mockGH, dir string, extraEnv ...string) (string, map[string]string) {
	t.Helper()
	outFileParent := t.TempDir()
	outFile := filepath.Join(outFileParent, "github_output")
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
	if dir != "" {
		cmd.Dir = dir
	}
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
// with the machine-greppable verdict marker for the given event+sha. LEGACY
// body shape (no ledger block) — review gate v1 rejects this shape with
// delivered=false + rejection_reason (pinned by
// TestCheckVerdict_RejectsVerdictWithoutLedgerBlock); kept because several
// pins deliberately exercise it (human-forged markers, dumped comments).
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

// --- Review gate v1 fixtures (docs/review-gate-v1-spec.md §4) -----------------
//
// PIN UPDATE NOTE (review-gate v1): the six delivery-channel pins below
// (fallback comment, request-changes marker, official review, review
// precedence, pagination, channel logs) originally used ledger-less bodies.
// Under the spec those bodies are now the REJECTION cell — "delivered"
// requires a parseable REVIEW-LEDGER v1 block — so the fixtures gained the
// block (and GATE_CHANGED_FILES for the coverage check). Each pin's INTENT
// is unchanged: two-channel precedence, sha freshness, pagination, and the
// channel-resolution log contract are asserted exactly as before, just
// against post-gate verdict bodies. The ledger-less shape is pinned as a
// rejection in TestCheckVerdict_RejectsVerdictWithoutLedgerBlock.

// ledgerVerdict maps a delivery event (APPROVE|REQUEST_CHANGES) to the
// ledger's lowercase verdict field.
func ledgerVerdict(event string) string {
	if event == "APPROVE" {
		return "approve"
	}
	return "request-changes"
}

// gateTestEnv pins the coverage check's hermeticity: the runner CWD is the
// ai-workflows checkout, whose REAL merge-base diff must never leak into
// fixtures — every validation run overrides the changed-file list (the
// GATE_CHANGED_FILES override in check-verdict.sh) to match the fixture's
// coverage line.
var gateTestEnv = []string{"GATE_CHANGED_FILES=README.md"}

// ledgerBlockV1 renders a minimal valid REVIEW-LEDGER v1 block: round 1,
// head pinned to headSHA (the runner env's PR_HEAD_SHA), the given
// lane/verdict, coverage covering gateTestEnv's changed-file list, plus any
// extra finding/waive lines verbatim.
func ledgerBlockV1(lane, verdict string, extraLines ...string) string {
	lines := []string{
		"REVIEW-LEDGER v1",
		"round: 1",
		"head: " + headSHA,
		"lane: " + lane,
		"verdict: " + verdict,
		"coverage: README.md",
	}
	lines = append(lines, extraLines...)
	lines = append(lines, "END-REVIEW-LEDGER")
	return strings.Join(lines, "\n") + "\n"
}

// markerBodyV1 wraps the legacy marker body shape with the mandatory ledger
// block — the v1 contract: every delivered verdict body carries exactly one
// parseable block.
func markerBodyV1(event, sha string) string {
	return markerCommentBody(event, sha) + "\n" + ledgerBlockV1("full", ledgerVerdict(event))
}

// markerCommentV1 is markerComment with the v1 ledger block included.
func markerCommentV1(event, sha string) string {
	return rawComment("github-actions[bot]", markerBodyV1(event, sha))
}

// reviewWithBody returns a reviews-array JSON element for an official bot
// review in the given state on sha, carrying body (the validation input the
// script reads via the --jq base64 extraction).
func reviewWithBody(state, sha, body string) string {
	b, _ := json.Marshal(map[string]any{
		"user":      map[string]any{"login": "github-actions[bot]"},
		"state":     state,
		"commit_id": sha,
		"body":      body,
	})
	return string(b)
}

// PIN 1 (owner ruling 6209846676): when the official review channel is
// platform-refused (422 approve-block, issue #46) and the reviewer posted
// the verdict as a marker comment, the gate MUST count it as delivered.
// This is the first pin of the fallback matrix. Fixture carries the v1
// ledger block (see the PIN UPDATE NOTE above).
func TestCheckVerdict_ApproveRefusedFallsBackToComment(t *testing.T) {
	m := newCheckMockGH(t, []string{"[" + markerCommentV1("APPROVE", headSHA) + "]"}, "[]")
	out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "comment" {
		t.Errorf("marker comment (APPROVE, matching sha, valid ledger) must deliver via channel 'comment', got %q\noutput:\n%s", outs["delivered"], out)
	}
	if outs["rejection_reason"] != "" {
		t.Errorf("a valid verdict must not carry rejection_reason, got %q", outs["rejection_reason"])
	}
}

func TestCheckVerdict_RequestChangesMarkerAlsoDelivers(t *testing.T) {
	m := newCheckMockGH(t, []string{"[" + markerCommentV1("REQUEST_CHANGES", headSHA) + "]"}, "[]")
	out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "comment" {
		t.Errorf("marker comment (REQUEST_CHANGES, matching sha, valid ledger) must deliver via channel 'comment', got %q\noutput:\n%s", outs["delivered"], out)
	}
}

// Official review wins over (or alongside) any comment: the review channel
// is authoritative when it exists. Review body validated through the same
// gate (v1 ledger required).
func TestCheckVerdict_OfficialReviewDelivers(t *testing.T) {
	body := "**Commit reviewed:** `" + headSHA + "`\n\n" + ledgerBlockV1("full", "approve")
	reviews := "[" + reviewWithBody("APPROVED", headSHA, body) + "]"
	m := newCheckMockGH(t, []string{"[]"}, reviews)
	out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "review" {
		t.Errorf("official bot APPROVED on head SHA (valid ledger body) must deliver via channel 'review', got %q\noutput:\n%s", outs["delivered"], out)
	}
}

// Review AND marker comment both present: review channel is reported (the
// comment is redundant, never a downgrade). Both bodies carry the v1 ledger.
func TestCheckVerdict_ReviewWinsOverComment(t *testing.T) {
	body := "**Commit reviewed:** `" + headSHA + "`\n\n" + ledgerBlockV1("full", "request-changes")
	reviews := "[" + reviewWithBody("CHANGES_REQUESTED", headSHA, body) + "]"
	m := newCheckMockGH(t, []string{"[" + markerCommentV1("APPROVE", headSHA) + "]"}, reviews)
	_, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "review" {
		t.Errorf("official review must take precedence over a marker comment, got %q", outs["delivered"])
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
// salvage C1 regression). The page-2 marker carries the v1 ledger.
func TestCheckVerdict_MarkerPaginates(t *testing.T) {
	page1 := "[" + rawComment("github-actions[bot]", "noise") + "]"
	page2 := "[" + markerCommentV1("APPROVE", headSHA) + "]"
	m := newCheckMockGH(t, []string{page1, page2}, "[]")
	out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "comment" {
		t.Errorf("marker on page 2 must still deliver, got %q\noutput:\n%s", outs["delivered"], out)
	}
}

// The channel-resolution log lines are the production e2e proof (owner
// ruling point 4) AND the gate-parsed contract (review round 2, F2): both
// verify steps extract the channel via sed from the EXACT line
// `Verdict delivery channel: <channel>` — pin the full format for all three
// channels so a cosmetic reword fails HERE, not as a fleet-wide false-red.
func TestCheckVerdict_EmitsChannelResolutionLogs(t *testing.T) {
	reviewBody := "**Commit reviewed:** `" + headSHA + "`\n\n" + ledgerBlockV1("full", "approve")
	cases := []struct {
		name    string
		pages   []string
		reviews string
		want    string
	}{
		{"review", []string{"[]"}, "[" + reviewWithBody("APPROVED", headSHA, reviewBody) + "]", "review"},
		{"comment", []string{"[" + markerCommentV1("APPROVE", headSHA) + "]"}, "[]", "comment"},
		{"false", []string{"[]"}, "[]", "false"},
	}
	reLine := regexp.MustCompile(`(?m)^Verdict delivery channel: (review|comment|false)\b`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newCheckMockGH(t, tc.pages, tc.reviews)
			out, _ := runCheckVerdictEnv(t, m, gateTestEnv...)
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

// PIN (lever 4 coupling): run_minutes must SUBTRACT the debounce quiet
// window — PR #69's structural-failure gate measures the first pass's own
// lifetime, never the lever-4 sleep. Without this, the default 10-minute
// debounce makes every structurally-doomed first pass read >=10 minutes
// and the >=3 retry gate never fires (the gate defeated by the debounce).
func TestCheckVerdict_RunMinutesSubtractsDebounce(t *testing.T) {
	t.Run("11m elapsed minus 600s debounce", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[]"}, "[]")
		writeJobsFixture(t, m, time.Now().Add(-11*time.Minute).UTC().Format(time.RFC3339))
		out, outs := runCheckVerdictEnv(t, m, "GITHUB_RUN_ID=12345", "DEBOUNCE_SECONDS=600")
		rm, ok := outs["run_minutes"]
		if !ok {
			t.Fatalf("expected run_minutes emission, got none\noutput:\n%s", out)
		}
		n := 0
		if _, err := fmt.Sscanf(rm, "%d", &n); err != nil {
			t.Fatalf("run_minutes must be an integer, got %q", rm)
		}
		if n < 0 || n > 1 {
			t.Errorf("11m elapsed minus a 600s debounce must floor to 0..1 (the pass itself lived ~1m — gate must be able to bite), got %d", n)
		}
	})
	t.Run("garbage DEBOUNCE_SECONDS fails open to zero", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[]"}, "[]")
		writeJobsFixture(t, m, time.Now().Add(-5*time.Minute).UTC().Format(time.RFC3339))
		out, outs := runCheckVerdictEnv(t, m, "GITHUB_RUN_ID=12345", "DEBOUNCE_SECONDS=not-a-number")
		rm, ok := outs["run_minutes"]
		if !ok {
			t.Fatalf("expected run_minutes emission, got none\noutput:\n%s", out)
		}
		n := 0
		if _, err := fmt.Sscanf(rm, "%d", &n); err != nil {
			t.Fatalf("run_minutes must be an integer, got %q", rm)
		}
		if n < 4 || n > 5 {
			t.Errorf("garbage DEBOUNCE_SECONDS must be treated as 0 (fail-open): 5m elapsed should floor to 4..5, got %d", n)
		}
	})
	t.Run("unset DEBOUNCE_SECONDS behaves as before", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[]"}, "[]")
		writeJobsFixture(t, m, time.Now().Add(-5*time.Minute).UTC().Format(time.RFC3339))
		_, outs := runCheckVerdictEnv(t, m, "GITHUB_RUN_ID=12345")
		n := 0
		if _, err := fmt.Sscanf(outs["run_minutes"], "%d", &n); err != nil {
			t.Fatalf("run_minutes must be an integer, got %q", outs["run_minutes"])
		}
		if n < 4 || n > 5 {
			t.Errorf("no debounce env => no subtraction: 5m elapsed should floor to 4..5, got %d", n)
		}
	})
}

// --- Review gate v1: verdict-body validation (docs/review-gate-v1-spec.md §4) --
//
// The rejection path is a DELIBERATE decision cell: delivered=false +
// rejection_reason=<machine-readable one-liner>, exit 0 (a decision was
// reached — the exit-code contract holds), and the stdout channel line
// reads false so both verify gates (sed-parsed) fail the job. The reason
// feeds pr-review.yml's bounded retry prompt.

// PIN: a ledger-less verdict body is the rejection cell. This is the exact
// shape the six delivery pins above used pre-v1 — the contract flip is
// pinned HERE so reverting the gate cannot pass silently.
func TestCheckVerdict_RejectsVerdictWithoutLedgerBlock(t *testing.T) {
	m := newCheckMockGH(t, []string{"[" + markerComment("APPROVE", headSHA) + "]"}, "[]")
	out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "false" {
		t.Errorf("ledger-less verdict must be rejected (delivered=false), got %q\noutput:\n%s", outs["delivered"], out)
	}
	if outs["rejection_reason"] != "missing-ledger-block" {
		t.Errorf("rejection_reason must be the machine-readable 'missing-ledger-block', got %q", outs["rejection_reason"])
	}
	if !strings.Contains(out, "Verdict delivery channel: false") {
		t.Errorf("rejection must report channel false on stdout (verify gates sed-parse it), got:\n%s", out)
	}
}

// PIN: exactly-one-block — a duplicate ledger block is malformed, not two
// chances to pass.
func TestCheckVerdict_RejectsDuplicateLedgerBlock(t *testing.T) {
	body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("full", "approve") + "\n" + ledgerBlockV1("full", "approve")
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
	out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "false" || !strings.HasPrefix(outs["rejection_reason"], "duplicate-ledger-block:") {
		t.Errorf("duplicate ledger blocks must reject with duplicate-ledger-block:<n>, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
	}
}

// PIN: the ledger `head:` must match the PR head at prompt-build time — a
// verdict pinned to another commit is stale even when freshly posted.
func TestCheckVerdict_RejectsHeadMismatch(t *testing.T) {
	body := markerCommentBody("APPROVE", headSHA) + "\n" + strings.Replace(ledgerBlockV1("full", "approve"), "head: "+headSHA, "head: "+otherSHA, 1)
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
	out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "false" || outs["rejection_reason"] != "head-mismatch" {
		t.Errorf("ledger head != PR_HEAD_SHA must reject with head-mismatch, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
	}
}

// PIN: `round:` must match the INJECTED round (GATE_ROUND) — and the check
// is skipped when the caller never injected one (e.g. ai-comment.yml's
// /review tail), so legacy callers keep working.
func TestCheckVerdict_RoundCheckGatedOnInjection(t *testing.T) {
	body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("full", "approve") // round: 1
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
	t.Run("injected round mismatch rejects", func(t *testing.T) {
		out, outs := runCheckVerdictEnv(t, m, append(gateTestEnv, "GATE_ROUND=5")...)
		if outs["delivered"] != "false" || outs["rejection_reason"] != "round-mismatch" {
			t.Errorf("round 1 vs injected GATE_ROUND=5 must reject with round-mismatch, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
	})
	t.Run("unset GATE_ROUND skips the check", func(t *testing.T) {
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "comment" {
			t.Errorf("without an injected round the verdict must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
}

// PIN: `lane:` must match the injected lane (GATE_LANE); unset skips.
func TestCheckVerdict_LaneCheckGatedOnInjection(t *testing.T) {
	body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("full", "approve")
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
	t.Run("injected lane mismatch rejects", func(t *testing.T) {
		out, outs := runCheckVerdictEnv(t, m, append(gateTestEnv, "GATE_LANE=docs")...)
		if outs["delivered"] != "false" || outs["rejection_reason"] != "lane-mismatch" {
			t.Errorf("ledger lane=full vs injected GATE_LANE=docs must reject with lane-mismatch, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
	})
	t.Run("matching injected lane delivers", func(t *testing.T) {
		out, outs := runCheckVerdictEnv(t, m, append(gateTestEnv, "GATE_LANE=full")...)
		if outs["delivered"] != "comment" {
			t.Errorf("matching lane must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
}

// PIN (spec §4b, design principle 2): the SCRIPT computes the open-blocking
// count — `verdict: approve` is valid only when it is 0. A correctness
// finding with status=new (open blocking) makes approve invalid, while the
// same body with request-changes is honest and delivers. status=fixed
// closes a finding; advisory classes (style) never block approve.
func TestCheckVerdict_ApproveWithOpenBlockingRejected(t *testing.T) {
	correctness := "finding: id=nil-deref; class=correctness; anchor=issue:3; pointer=checkverdict_test.go:4; status=new; risk=nil deref; fix_cost=guard clause"
	t.Run("approve with open correctness is rejected", func(t *testing.T) {
		body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("full", "approve", correctness)
		m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "false" || outs["rejection_reason"] != "approve-with-open-blocking:1" {
			t.Errorf("approve with an open blocking-class finding must reject with approve-with-open-blocking:<n>, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
	})
	t.Run("request-changes with the same finding delivers", func(t *testing.T) {
		body := markerCommentBody("REQUEST_CHANGES", headSHA) + "\n" + ledgerBlockV1("full", "request-changes", correctness)
		m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "comment" {
			t.Errorf("request-changes with open findings is honest and must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
	t.Run("fixed finding does not block approve", func(t *testing.T) {
		fixed := strings.Replace(correctness, "status=new", "status=fixed", 1)
		body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("full", "approve", fixed)
		m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "comment" {
			t.Errorf("status=fixed closes the finding; approve must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
	t.Run("open advisory finding does not block approve", func(t *testing.T) {
		style := "finding: id=naming; class=style; anchor=rule:2; pointer=checkverdict_test.go:4; status=still-open; risk=naming; fix_cost=rename"
		body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("full", "approve", style)
		m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "comment" {
			t.Errorf("open advisory (style) must not block approve — convergence gates merging, not reviewing, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
}

// PIN: the class vocabulary is closed — an invented class cannot participate
// in the script-owned map, so it invalidates the verdict (the reviewer
// prompt promises exactly this).
func TestCheckVerdict_UnknownClassRejected(t *testing.T) {
	line := "finding: id=weird; class=sev1; anchor=issue:1; pointer=checkverdict_test.go:4; status=new; risk=x; fix_cost=y"
	body := markerCommentBody("REQUEST_CHANGES", headSHA) + "\n" + ledgerBlockV1("full", "request-changes", line)
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
	out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "false" || outs["rejection_reason"] != "unknown-class:sev1" {
		t.Errorf("invented class must reject with unknown-class:<class>, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
	}
}

// PIN: anchors are a closed list (issue:<n>, rule:<n>, claim:"...") —
// anything else is an invalid verdict (design principle 3).
func TestCheckVerdict_InvalidAnchorRejected(t *testing.T) {
	line := `finding: id=no-anchor; class=style; anchor=issue-3; pointer=checkverdict_test.go:4; status=new; risk=x; fix_cost=y`
	body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("full", "approve", line)
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
	out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
	if outs["delivered"] != "false" || !strings.HasPrefix(outs["rejection_reason"], "invalid-anchor:") {
		t.Errorf("anchor outside the closed list must reject with invalid-anchor:<anchor>, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
	}
}

// PIN (spec §Waivers): body `waive:` lines mirror the author's waiver
// comments; they close advisory findings only. A waive naming a
// blocking-class id closes NOTHING — approve stays invalid (reconcile-
// ledger.sh surfaces it as rejected_waivers at merge time).
func TestCheckVerdict_WaivesCloseAdvisoryOnly(t *testing.T) {
	t.Run("advisory waive honored", func(t *testing.T) {
		style := "finding: id=naming; class=style; anchor=rule:2; pointer=checkverdict_test.go:4; status=still-open; risk=naming; fix_cost=rename"
		waive := "waive: id=naming; reason=accepted for v1"
		body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("full", "approve", style, waive)
		m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "comment" {
			t.Errorf("waived advisory finding must not block approve, got %q\noutput:\n%s", outs["delivered"], out)
		}
		if !strings.Contains(out, "waive line(s)") {
			t.Errorf("the waive mirror should be logged for the audit trail, got:\n%s", out)
		}
	})
	t.Run("blocking-class waive ignored", func(t *testing.T) {
		correctness := "finding: id=nil-deref; class=correctness; anchor=issue:3; pointer=checkverdict_test.go:4; status=new; risk=nil deref; fix_cost=guard"
		waive := "waive: id=nil-deref; reason=trust me"
		body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("full", "approve", correctness, waive)
		m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "false" || outs["rejection_reason"] != "approve-with-open-blocking:1" {
			t.Errorf("a waive must never close a blocking-class finding (author-waivable = advisory only), got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
	})
}

// PIN (design principle 7 / §4b): docs-lane verdicts may not raise findings
// about non-docs files — path mismatch is an invalid verdict. A docs finding
// on a docs path delivers.
func TestCheckVerdict_DocsLaneFindingScope(t *testing.T) {
	t.Run("finding on a code path rejects", func(t *testing.T) {
		line := "finding: id=stale-doc; class=docs-truth; anchor=rule:2; pointer=internal/bot/session.go:12; status=new; risk=stale; fix_cost=edit"
		body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("docs", "approve", line)
		m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, "GATE_CHANGED_FILES=docs/a.md")
		if outs["delivered"] != "false" || outs["rejection_reason"] != "docs-lane-finding:internal/bot/session.go" {
			t.Errorf("docs-lane finding on a non-docs path must reject with docs-lane-finding:<path>, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
	})
	t.Run("finding on a docs path delivers", func(t *testing.T) {
		line := "finding: id=stale-doc; class=docs-truth; anchor=rule:2; pointer=../../README.md:1; status=new; risk=stale; fix_cost=edit"
		body := markerCommentBody("REQUEST_CHANGES", headSHA) + "\n" + ledgerBlockV1("docs", "request-changes", line)
		m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, "GATE_CHANGED_FILES=docs/a.md")
		if outs["delivered"] != "comment" {
			t.Errorf("docs-lane finding on a docs path must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
}

// PIN (spec §4c): docs lane additionally requires EVERY changed file to be
// docs-lane-allowed — a code file in a docs-lane diff invalidates the lane
// itself.
func TestCheckVerdict_DocsLaneChangedFilesScope(t *testing.T) {
	body := markerCommentBody("APPROVE", headSHA) + "\n" + ledgerBlockV1("docs", "approve")
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
	out, outs := runCheckVerdictEnv(t, m, "GATE_CHANGED_FILES=docs/a.md src/main.go")
	if outs["delivered"] != "false" || outs["rejection_reason"] != "docs-lane-scope:src/main.go" {
		t.Errorf("docs lane with a code file changed must reject with docs-lane-scope:<file>, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
	}
}

// PIN (spec §4c, coverage contract): full lane → every changed file appears
// on a coverage: line. A missing file rejects with the (≤3-file, machine-
// readable) gap list; full coverage delivers.
func TestCheckVerdict_CoverageVsChangedFiles(t *testing.T) {
	t.Run("coverage gap rejects", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[" + markerCommentV1("APPROVE", headSHA) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, "GATE_CHANGED_FILES=README.md src/main.go docs/x.md")
		if outs["delivered"] != "false" || !strings.HasPrefix(outs["rejection_reason"], "coverage-missing:") {
			t.Errorf("unchanged-listed files must reject with coverage-missing:<files>, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
		if !strings.Contains(outs["rejection_reason"], "src/main.go") {
			t.Errorf("the gap list must name the uncovered changed files, got %q", outs["rejection_reason"])
		}
	})
	t.Run("full coverage delivers", func(t *testing.T) {
		m := newCheckMockGH(t, []string{"[" + markerCommentV1("APPROVE", headSHA) + "]"}, "[]")
		out, outs := runCheckVerdictEnv(t, m, "GATE_CHANGED_FILES=README.md")
		if outs["delivered"] != "comment" {
			t.Errorf("coverage covering every changed file must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
}

// PIN: the coverage check FAILS OPEN when the changed-file list is
// unobtainable (no GATE_CHANGED_FILES override and no git repo in CWD) — a
// tooling gap must never reject an honest verdict. The temp dir has no .git,
// so the script's merge-base diff is unavailable and the check is skipped
// with a logged note.
func TestCheckVerdict_CoverageCheckFailsOpenWithoutDiff(t *testing.T) {
	m := newCheckMockGH(t, []string{"[" + markerCommentV1("APPROVE", headSHA) + "]"}, "[]")
	out, outs := runCheckVerdictEnvDir(t, m, t.TempDir())
	if outs["delivered"] != "comment" {
		t.Errorf("unobtainable changed-file list must skip the coverage check (fail-open), got delivered=%q\noutput:\n%s", outs["delivered"], out)
	}
	if !strings.Contains(out, "coverage check skipped") {
		t.Errorf("the skip must be visible in the log (never silent), got:\n%s", out)
	}
}

// PIN (spec §4d): pointer spot-check — sampled finding pointers must exist
// in the checkout with the line within file length. Hallucinated paths and
// out-of-range lines reject. The worktree check counts a final line WITHOUT
// a trailing newline (awk NR, not wc -l): a pointer at that line is valid.
func TestCheckVerdict_PointerSpotCheck(t *testing.T) {
	dir := t.TempDir()
	threeLinesNoFinalNewline := "line one\nline two\nline three"
	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte(threeLinesNoFinalNewline), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, pointer string) (string, map[string]string) {
		t.Helper()
		line := fmt.Sprintf("finding: id=x; class=correctness; anchor=issue:1; pointer=%s; status=new; risk=x; fix_cost=y", pointer)
		// Coverage must match THIS fixture's changed-file list (code.go),
		// not ledgerBlockV1's README.md default.
		block := strings.Replace(ledgerBlockV1("full", "request-changes", line), "coverage: README.md", "coverage: code.go", 1)
		body := markerCommentBody("REQUEST_CHANGES", headSHA) + "\n" + block
		m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
		return runCheckVerdictEnvDir(t, m, dir, "GATE_CHANGED_FILES=code.go")
	}
	t.Run("valid pointer delivers", func(t *testing.T) {
		out, outs := run(t, "code.go:2")
		if outs["delivered"] != "comment" {
			t.Errorf("pointer into an existing file, line within length, must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
	t.Run("final line without trailing newline is in range", func(t *testing.T) {
		out, outs := run(t, "code.go:3")
		if outs["delivered"] != "comment" {
			t.Errorf("awk NR counts the unterminated final line; pointer code.go:3 must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
	t.Run("hallucinated path rejects", func(t *testing.T) {
		out, outs := run(t, "ghost.go:1")
		if outs["delivered"] != "false" || outs["rejection_reason"] != "invalid-pointer:ghost.go:1" {
			t.Errorf("nonexistent pointer path must reject with invalid-pointer:<path>:<line>, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
	})
	t.Run("line beyond EOF rejects", func(t *testing.T) {
		out, outs := run(t, "code.go:99")
		if outs["delivered"] != "false" || outs["rejection_reason"] != "invalid-pointer:code.go:99" {
			t.Errorf("out-of-range pointer line must reject, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
	})
}

// PIN (spec §4d "sample up to 3"): the spot-check is bounded — the FIRST
// three sampled pointers (blocking-class first) decide. Findings 4+ are NOT
// spot-checked, so invalid deep pointers cannot fail an otherwise honest
// verdict whose sampled anchors check out.
func TestCheckVerdict_PointerSamplingCapsAtThree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("x\ny\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Four blocking findings: pointers 1-3 valid, pointer 4 hallucinated.
	findings := []string{
		"finding: id=f1; class=correctness; anchor=issue:1; pointer=a.go:1; status=new; risk=x; fix_cost=y",
		"finding: id=f2; class=robustness; anchor=rule:1; pointer=a.go:2; status=new; risk=x; fix_cost=y",
		"finding: id=f3; class=security; anchor=issue:2; pointer=a.go:1; status=new; risk=x; fix_cost=y",
		"finding: id=f4; class=test-integrity; anchor=rule:2; pointer=ghost.go:1; status=new; risk=x; fix_cost=y",
	}
	// Coverage matches THIS fixture's changed-file list (a.go).
	block := strings.Replace(ledgerBlockV1("full", "request-changes", findings...), "coverage: README.md", "coverage: a.go", 1)
	body := markerCommentBody("REQUEST_CHANGES", headSHA) + "\n" + block
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
	out, outs := runCheckVerdictEnvDir(t, m, dir, "GATE_CHANGED_FILES=a.go")
	if outs["delivered"] != "comment" {
		t.Errorf("spot-check must sample at most 3 pointers — f4's hallucinated pointer is beyond the cap, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
	}
}

// PIN (spec §4e, design principle 6): any ci=<state> claim in the body must
// match the INJECTED GATE_CI_STATUS. Claiming green while CI is failing is
// a dishonest verdict; matching claims deliver; unset injection skips the
// check.
func TestCheckVerdict_CiClaimsVsInjection(t *testing.T) {
	body := markerCommentBody("APPROVE", headSHA) + "\nci=success per the injected status\n" + ledgerBlockV1("full", "approve")
	m := newCheckMockGH(t, []string{"[" + rawComment("github-actions[bot]", body) + "]"}, "[]")
	t.Run("claim contradicting the injection rejects", func(t *testing.T) {
		out, outs := runCheckVerdictEnv(t, m, append(gateTestEnv, "GATE_CI_STATUS=failure")...)
		if outs["delivered"] != "false" || outs["rejection_reason"] != "ci-claim-mismatch:success" {
			t.Errorf("ci=success claim vs injected failure must reject with ci-claim-mismatch:<claimed>, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
	})
	t.Run("matching claim delivers", func(t *testing.T) {
		out, outs := runCheckVerdictEnv(t, m, append(gateTestEnv, "GATE_CI_STATUS=success")...)
		if outs["delivered"] != "comment" {
			t.Errorf("claim matching the injection must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
	t.Run("unset injection skips the check", func(t *testing.T) {
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "comment" {
			t.Errorf("without an injected CI status the claim check must be skipped, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
}

// PIN: when several official reviews match the head SHA, the LAST (newest —
// GitHub returns reviews oldest-first) body is the operative verdict that
// gets validated, not the first.
func TestCheckVerdict_LatestReviewBodyValidated(t *testing.T) {
	good := "**Commit reviewed:** `" + headSHA + "`\n\n" + ledgerBlockV1("full", "approve")
	bad := "**Commit reviewed:** `" + headSHA + "`\n\nno ledger in the older review"
	t.Run("newest body valid => delivered", func(t *testing.T) {
		reviews := "[" + reviewWithBody("APPROVED", headSHA, bad) + "," + reviewWithBody("APPROVED", headSHA, good) + "]"
		m := newCheckMockGH(t, []string{"[]"}, reviews)
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "review" {
			t.Errorf("the newest review body decides; a valid newest body must deliver, got %q\noutput:\n%s", outs["delivered"], out)
		}
	})
	t.Run("newest body invalid => rejected", func(t *testing.T) {
		reviews := "[" + reviewWithBody("APPROVED", headSHA, good) + "," + reviewWithBody("APPROVED", headSHA, bad) + "]"
		m := newCheckMockGH(t, []string{"[]"}, reviews)
		out, outs := runCheckVerdictEnv(t, m, gateTestEnv...)
		if outs["delivered"] != "false" || outs["rejection_reason"] != "missing-ledger-block" {
			t.Errorf("an invalid NEWEST body must reject even when an older valid one exists, got delivered=%q reason=%q\noutput:\n%s", outs["delivered"], outs["rejection_reason"], out)
		}
	})
}
