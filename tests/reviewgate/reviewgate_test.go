// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package reviewgate exercises the review-gate v1 script layer
// (docs/review-gate-v1-spec.md): scripts/review-gate/classify-lane.sh,
// review-state.sh, and reconcile-ledger.sh.
//
// The scripts are the deterministic half of the review loop — counts,
// parsing, class→blocking mapping, waiver rules, the convergence predicate —
// so these tests are golden fixture tests: every gh api response is served
// from a fixture directory via REVIEW_STATE_FROM_FILE / --from-file, and
// classify-lane gets a hand-built repo tree plus a `git diff --name-only`
// listing. No network, no gh, no token (mirroring the mock-gh subprocess
// style of tests/gharouter, minus the mock — the fixture override replaces
// it wholesale).
//
// Byte-stability is pinned directly: reconcile-ledger re-run on the same
// fixtures must produce byte-identical JSON, because /merge refuses unless
// converged=true and any nondeterminism would make the gate flake.
package reviewgate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	gateHeadSHA  = "abcdef0123456789abcdef0123456789abcdef01"
	gateOtherSHA = "1111111111111111111111111111111111111111"
	gateBotActor = "github-actions[bot]"
	gateAuthor   = "lenaxia"
)

// gateScriptPath resolves scripts/review-gate/<name> relative to the repo.
func gateScriptPath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "review-gate", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s not found at %s: %v", name, p, err)
	}
	return p
}

// runGateScript executes a review-gate script as a subprocess and returns
// stdout, stderr and the run error separately: reconcile's contract puts
// {"error": ...} on STDOUT with a non-zero exit, and classify-lane's contract
// is one stdout line, so callers must be able to assert on both channels.
func runGateScript(t *testing.T, script, workDir string, env []string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(script, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// writeGateFixtures materializes a fixture directory for
// REVIEW_STATE_FROM_FILE / --from-file: name → verbatim file content.
func writeGateFixtures(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

// writeGateTree materializes a fake working tree for classify-lane's shebang
// check (paths in the diff listing are resolved against the script's cwd).
func writeGateTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal fixture: %v", err)) // unreachable for map[string]any fixtures
	}
	return string(b)
}

func gateReviewJSON(login, state, commitID, body string) string {
	return mustJSON(map[string]any{
		"user": map[string]any{"login": login}, "state": state,
		"commit_id": commitID, "body": body,
	})
}

func gateCommentJSON(login, body string) string {
	return mustJSON(map[string]any{
		"user": map[string]any{"login": login}, "body": body,
	})
}

// gateMarkerBody is the verdict-marker comment shape check-verdict.sh greps:
// machine marker first, then the human-readable sections.
func gateMarkerBody(event, sha string) string {
	return fmt.Sprintf("<!-- ai-workflows-verdict: %s sha=%s -->\n**Commit reviewed:** `%s`\n", event, sha, sha)
}

// gateLedgerBody wraps a REVIEW-LEDGER v1 block in a verdict body. verdict is
// the spec spelling (approve|request-changes); the marker event is derived.
func gateLedgerBody(verdict, head string, round int, lane string, findings, waives []string) string {
	event := "APPROVE"
	if verdict == "request-changes" {
		event = "REQUEST_CHANGES"
	}
	var b strings.Builder
	b.WriteString(gateMarkerBody(event, head))
	b.WriteString("\nREVIEW-LEDGER v1\n")
	fmt.Fprintf(&b, "round: %d\nhead: %s\nlane: %s\nverdict: %s\n", round, head, lane, verdict)
	b.WriteString("coverage: src/x.go, docs/a.md\n")
	for _, f := range findings {
		b.WriteString("finding: " + f + "\n")
	}
	for _, w := range waives {
		b.WriteString("waive: " + w + "\n")
	}
	b.WriteString("END-REVIEW-LEDGER\n")
	return b.String()
}

// gateFinding renders one finding line per the spec grammar (id/class/anchor/
// pointer/status/risk required; fix_cost is the optional, unconsumed field).
func gateFinding(id, class, anchor, pointer, status, risk string) string {
	return fmt.Sprintf("id=%s; class=%s; anchor=%s; pointer=%s; status=%s; risk=%s; fix_cost=one line",
		id, class, anchor, pointer, status, risk)
}

func gateWaiverComment(id, reason string) string {
	return gateCommentJSON(gateAuthor, fmt.Sprintf("waive %s: %s\n", id, reason))
}

// --- classify-lane.sh ---------------------------------------------------------

// TestClassifyLane_GoldenTable pins the spec §1 predicate over a diff
// listing: docs iff EVERY path is docs-ish AND no changed file is executable
// (extension or shebang). Empty diff → docs (vacuous all()); the shebang and
// hidden-.sh clauses plug the "docs lane ships live code" escape hatch.
func TestClassifyLane_GoldenTable(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string // fake tree for shebang checks
		listing []string          // git diff --name-only output lines
		want    string
	}{
		{
			name:    "docs-only md set is docs",
			files:   map[string]string{"docs/a.md": "x", "README.md": "x", "worklogs/w.md": "x", ".github/prompts/review.md": "x"},
			listing: []string{"docs/a.md", "README.md", "worklogs/w.md", ".github/prompts/review.md"},
			want:    "lane=docs",
		},
		{
			name:    "md plus one sh is full",
			files:   map[string]string{"docs/a.md": "x", "scripts/run.sh": "#!/bin/sh\n"},
			listing: []string{"docs/a.md", "scripts/run.sh"},
			want:    "lane=full",
		},
		{
			name:    "design dir with hidden sh is full",
			files:   map[string]string{"design/notes.md": "x", "design/helper.sh": "x"},
			listing: []string{"design/notes.md", "design/helper.sh"},
			want:    "lane=full",
		},
		{
			name:    "empty diff is docs",
			files:   map[string]string{},
			listing: nil,
			want:    "lane=docs",
		},
		{
			name:    "path with spaces stays docs",
			files:   map[string]string{"docs/my file.md": "x"},
			listing: []string{"docs/my file.md"},
			want:    "lane=docs",
		},
		{
			name:    "path with spaces and sh is full",
			files:   map[string]string{"docs/my file.md": "x", "scripts/my script.sh": "x"},
			listing: []string{"docs/my file.md", "scripts/my script.sh"},
			want:    "lane=full",
		},
		{
			name:    "shebang in extensionless docs file is full",
			files:   map[string]string{"docs/runbook": "#!/bin/sh\necho hi\n"},
			listing: []string{"docs/runbook"},
			want:    "lane=full",
		},
		{
			name:    "trailing slash dir entry is docs",
			files:   map[string]string{"docs/a.md": "x"},
			listing: []string{"docs/"},
			want:    "lane=docs",
		},
		{
			name:    "deleted docs file cannot carry shebang",
			files:   map[string]string{},
			listing: []string{"docs/removed.md"},
			want:    "lane=docs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := writeGateTree(t, tc.files)
			listingDir := t.TempDir()
			listing := filepath.Join(listingDir, "diff.txt")
			if err := os.WriteFile(listing, []byte(strings.Join(tc.listing, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, stderr, err := runGateScript(t, gateScriptPath(t, "classify-lane.sh"), repo, nil, "--diff-file", listing)
			if err != nil {
				t.Fatalf("classify-lane.sh must always exit 0 (total classification): %v\nstdout: %s\nstderr: %s", err, out, stderr)
			}
			if strings.TrimSpace(out) != tc.want {
				t.Errorf("got %q, want %q\nstderr: %s", strings.TrimSpace(out), tc.want, stderr)
			}
		})
	}
}

// TestClassifyLane_ReadsDiffFromStdin pins the stdin contract — the workflow
// pipes `git diff --name-only` straight in, no temp file.
func TestClassifyLane_ReadsDiffFromStdin(t *testing.T) {
	script := gateScriptPath(t, "classify-lane.sh")
	repo := writeGateTree(t, map[string]string{"docs/a.md": "x"})

	cmd := exec.Command(script)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader("docs/a.md\nREADME.md\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("stdin invocation failed: %v\nstderr: %s", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "lane=docs" {
		t.Errorf("stdin listing classified %q, want lane=docs", got)
	}
}

// --- review-state.sh ----------------------------------------------------------

// gateStateEnv builds the common invocation env: fixture dir + repo.
func gateStateEnv(fixtureDir string) []string {
	return []string{
		"REVIEW_STATE_FROM_FILE=" + fixtureDir,
		"REPOSITORY=lenaxia/test",
	}
}

// TestReviewState_RoundsCountsHeadPinnedVerdicts pins the rounds contract:
// formal bot reviews (Commit-reviewed line + commit_id == head) PLUS
// sha-pinned marker comments. Excluded: stale-sha reviews and markers (a push
// resets the verdict ledger for the new head), reviews without the
// Commit-reviewed line, and anything not authored by the bot actor.
func TestReviewState_RoundsCountsHeadPinnedVerdicts(t *testing.T) {
	reviews := "[" + strings.Join([]string{
		gateReviewJSON(gateBotActor, "CHANGES_REQUESTED", gateHeadSHA, "**Commit reviewed:** `"+gateHeadSHA+"`\n"),
		gateReviewJSON(gateBotActor, "APPROVED", gateOtherSHA, "**Commit reviewed:** stale review, stale sha"),
		gateReviewJSON(gateBotActor, "COMMENTED", gateHeadSHA, "no commit-reviewed line here"),
		gateReviewJSON(gateAuthor, "APPROVED", gateHeadSHA, "**Commit reviewed:** human, ignored"),
	}, ",") + "]"
	comments := "[" + strings.Join([]string{
		gateCommentJSON(gateBotActor, gateMarkerBody("APPROVE", gateHeadSHA)),
		gateCommentJSON(gateBotActor, gateMarkerBody("REQUEST_CHANGES", gateHeadSHA)),
		gateCommentJSON(gateBotActor, gateMarkerBody("APPROVE", gateOtherSHA)),
		gateCommentJSON(gateAuthor, gateMarkerBody("APPROVE", gateHeadSHA)),
		gateCommentJSON(gateBotActor, "## Code Review\n\nunmarked bot comment"),
	}, ",") + "]"

	t.Run("counts fresh reviews and markers", func(t *testing.T) {
		dir := writeGateFixtures(t, map[string]string{
			"pull.json":     fmt.Sprintf(`{"head":{"sha":%q},"user":{"login":%q}}`, gateHeadSHA, gateAuthor),
			"reviews.json":  reviews,
			"comments.json": comments,
		})
		out, stderr, err := runGateScript(t, gateScriptPath(t, "review-state.sh"), "", gateStateEnv(dir), "rounds", "42")
		if err != nil {
			t.Fatalf("rounds failed: %v\nstderr: %s", err, stderr)
		}
		// 1 fresh review + 2 fresh markers; stale sha, human, unmarked all excluded.
		if got := strings.TrimSpace(out); got != "3" {
			t.Errorf("rounds = %q, want 3\nstderr: %s", got, stderr)
		}
	})

	t.Run("empty PR is zero", func(t *testing.T) {
		dir := writeGateFixtures(t, map[string]string{
			"pull.json": fmt.Sprintf(`{"head":{"sha":%q},"user":{"login":%q}}`, gateHeadSHA, gateAuthor),
		})
		out, stderr, err := runGateScript(t, gateScriptPath(t, "review-state.sh"), "", gateStateEnv(dir), "rounds", "42")
		if err != nil {
			t.Fatalf("rounds failed: %v\nstderr: %s", err, stderr)
		}
		if got := strings.TrimSpace(out); got != "0" {
			t.Errorf("rounds on empty PR = %q, want 0\nstderr: %s", got, stderr)
		}
	})
}

// TestReviewState_CiStatus pins the ci-status priority ladder: any failure →
// failure (cancelled counts as failure — fail-closed); any unconcluded run →
// pending; zero runs → none; else success. Bare-array and workflow_runs
// wrapper fixture shapes are both accepted.
func TestReviewState_CiStatus(t *testing.T) {
	cases := []struct {
		name string
		runs string // runs.json content; "" = file absent
		want string
	}{
		{"any failure wins", `{"total_count":2,"workflow_runs":[{"conclusion":"success"},{"conclusion":"failure"}]}`, "ci=failure"},
		{"cancelled is failure not success", `[{"conclusion":"cancelled"}]`, "ci=failure"},
		{"running makes pending", `{"workflow_runs":[{"conclusion":null,"status":"in_progress"},{"conclusion":"success"}]}`, "ci=pending"},
		{"all concluded is success", `{"workflow_runs":[{"conclusion":"success"},{"conclusion":"skipped"}]}`, "ci=success"},
		{"bare array shape works", `[{"conclusion":"success"}]`, "ci=success"},
		{"no runs is none", "", "ci=none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			if tc.runs != "" {
				files["runs.json"] = tc.runs
			}
			dir := writeGateFixtures(t, files)
			out, stderr, err := runGateScript(t, gateScriptPath(t, "review-state.sh"), "", gateStateEnv(dir), "ci-status", gateHeadSHA)
			if err != nil {
				t.Fatalf("ci-status failed: %v\nstderr: %s", err, stderr)
			}
			if got := strings.TrimSpace(out); got != tc.want {
				t.Errorf("ci-status = %q, want %q\nstderr: %s", got, tc.want, stderr)
			}
		})
	}
}

// TestReviewState_HeadCheck pins the merge-gate freshness check: exit 0 iff
// the PR's current head sha equals the argument (the sha is always the full
// 40-hex from a ledger head: line).
func TestReviewState_HeadCheck(t *testing.T) {
	dir := writeGateFixtures(t, map[string]string{
		"pull.json": fmt.Sprintf(`{"head":{"sha":%q},"user":{"login":%q}}`, gateHeadSHA, gateAuthor),
	})
	env := gateStateEnv(dir)
	script := gateScriptPath(t, "review-state.sh")

	t.Run("matching sha exits 0", func(t *testing.T) {
		out, _, err := runGateScript(t, script, "", append(env, "PR_NUMBER=42"), "head-check", gateHeadSHA)
		if err != nil {
			t.Fatalf("head-check must exit 0 on match: %v\nstderr: %s", err, out)
		}
	})
	t.Run("mismatched sha exits 1", func(t *testing.T) {
		_, stderr, err := runGateScript(t, script, "", append(env, "PR_NUMBER=42"), "head-check", gateOtherSHA)
		if err == nil {
			t.Fatalf("head-check must exit non-zero on mismatch (stdout: %s)", stderr)
		}
	})
	t.Run("missing pr context is a usage error", func(t *testing.T) {
		_, _, err := runGateScript(t, script, "", env, "head-check", gateHeadSHA)
		if err == nil {
			t.Fatal("head-check without --pr/PR_NUMBER must fail (cannot resolve the PR)")
		}
	})
}

// --- reconcile-ledger.sh ------------------------------------------------------

// reconcileFixture builds the standard fixture dir: author lenaxia at
// gateHeadSHA; reviews/comments verbatim JSON arrays.
func reconcileFixture(t *testing.T, reviews, comments string) string {
	t.Helper()
	return writeGateFixtures(t, map[string]string{
		"pull.json":     fmt.Sprintf(`{"head":{"sha":%q},"user":{"login":%q}}`, gateHeadSHA, gateAuthor),
		"reviews.json":  reviews,
		"comments.json": comments,
	})
}

func runReconcile(t *testing.T, fixtureDir string, extraArgs ...string) (map[string]any, string, string, error) {
	t.Helper()
	args := append([]string{"42", "--from-file", fixtureDir}, extraArgs...)
	out, stderr, err := runGateScript(t, gateScriptPath(t, "reconcile-ledger.sh"), "", []string{"REPOSITORY=lenaxia/test"}, args...)
	var m map[string]any
	if jerr := json.Unmarshal([]byte(out), &m); jerr != nil {
		t.Fatalf("reconcile stdout is not JSON: %v\nstdout: %s\nstderr: %s", jerr, out, stderr)
	}
	return m, out, stderr, err
}

func reconStr(m map[string]any, k string) string { v, _ := m[k].(string); return v }
func reconBool(m map[string]any, k string) bool  { v, _ := m[k].(bool); return v }
func reconLen(m map[string]any, k string) int {
	v, ok := m[k].([]any)
	if !ok {
		return -1
	}
	return len(v)
}

func reconIDs(t *testing.T, m map[string]any, key string) []string {
	t.Helper()
	raw, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s missing or not an array: %v", key, m[key])
	}
	ids := make([]string, 0, len(raw))
	for _, e := range raw {
		obj, _ := e.(map[string]any)
		id, _ := obj["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// TestReconcileLedger_NoVerdictsConvergedEmptyLedger: an empty history is the
// converged empty ledger — the very first review of a PR must never be
// pre-blocked by a phantom ledger.
func TestReconcileLedger_NoVerdictsConvergedEmptyLedger(t *testing.T) {
	dir := reconcileFixture(t, "[]", "[]")
	m, out, stderr, err := runReconcile(t, dir)
	if err != nil {
		t.Fatalf("reconcile failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	if !reconBool(m, "converged") {
		t.Errorf("empty history must converge, got %v\nstdout: %s", m["converged"], out)
	}
	for _, key := range []string{"open_blocking", "open_advisory", "waived", "rejected_waivers"} {
		if got := reconLen(m, key); got != 0 {
			t.Errorf("%s must be empty on an empty history, got %d", key, got)
		}
	}
	if got := reconStr(m, "last_verdict"); got != "" {
		t.Errorf("last_verdict must be empty with no verdicts, got %q", got)
	}
	if got := reconStr(m, "head"); got != "" {
		t.Errorf("head must be empty with no verdicts, got %q", got)
	}
	if got := reconStr(m, "lane"); got != "full" {
		t.Errorf("default lane must be full, got %q", got)
	}
}

// TestReconcileLedger_RequestChangesNotConverged: the canonical mid-loop
// state — one request-changes verdict carrying 2 blocking + 1 advisory
// findings blocks the merge until they resolve.
func TestReconcileLedger_RequestChangesNotConverged(t *testing.T) {
	body := gateLedgerBody("request-changes", gateHeadSHA, 1, "full", []string{
		gateFinding("off-by-one", "correctness", "issue:3", "src/x.go:42", "new", "wrong output"),
		gateFinding("null-guard", "security", "rule:7", "src/y.go:10", "new", "panic on nil"),
		gateFinding("style-nit", "style", "rule:2", "src/x.go:1", "new", "naming"),
	}, nil)
	dir := reconcileFixture(t, "["+gateReviewJSON(gateBotActor, "CHANGES_REQUESTED", gateHeadSHA, body)+"]", "[]")
	m, out, _, err := runReconcile(t, dir)
	if err != nil {
		t.Fatalf("reconcile failed: %v\nstdout: %s", err, out)
	}
	if reconBool(m, "converged") {
		t.Errorf("open blocking findings must not converge\nstdout: %s", out)
	}
	if got := reconLen(m, "open_blocking"); got != 2 {
		t.Errorf("open_blocking = %d, want 2 (correctness + security)", got)
	}
	if got := reconLen(m, "open_advisory"); got != 1 {
		t.Errorf("open_advisory = %d, want 1 (style)", got)
	}
	if got := reconStr(m, "last_verdict"); got != "request-changes" {
		t.Errorf("last_verdict = %q, want request-changes", got)
	}
	if got := reconStr(m, "head"); got != gateHeadSHA {
		t.Errorf("head = %q, want the verdict's pinned head", got)
	}
	if got, ok := m["round"].(float64); !ok || got != 1 {
		t.Errorf("round = %v, want 1", m["round"])
	}
}

// TestReconcileLedger_AuthorWaiverAndFixesConverge: the convergence story —
// the author waives the advisory id, the next verdict marks the blocking ids
// fixed → converged=true, waived carries the author's reason.
func TestReconcileLedger_AuthorWaiverAndFixesConverge(t *testing.T) {
	round1 := gateLedgerBody("request-changes", gateHeadSHA, 1, "full", []string{
		gateFinding("off-by-one", "correctness", "issue:3", "src/x.go:42", "new", "wrong output"),
		gateFinding("null-guard", "security", "rule:7", "src/y.go:10", "new", "panic on nil"),
		gateFinding("style-nit", "style", "rule:2", "src/x.go:1", "new", "naming"),
	}, nil)
	round2 := gateLedgerBody("approve", gateHeadSHA, 2, "full", []string{
		gateFinding("off-by-one", "correctness", "issue:3", "src/x.go:42", "fixed", "wrong output"),
		gateFinding("null-guard", "security", "rule:7", "src/y.go:10", "fixed", "panic on nil"),
		gateFinding("style-nit", "style", "rule:2", "src/x.go:1", "still-open", "naming"),
	}, nil)
	reviews := "[" + gateReviewJSON(gateBotActor, "CHANGES_REQUESTED", gateHeadSHA, round1) + "]"
	comments := "[" + gateWaiverComment("style-nit", "cosmetic, fine for now") + "," +
		gateCommentJSON(gateBotActor, round2) + "]"
	dir := reconcileFixture(t, reviews, comments)
	m, out, _, err := runReconcile(t, dir)
	if err != nil {
		t.Fatalf("reconcile failed: %v\nstdout: %s", err, out)
	}
	if !reconBool(m, "converged") {
		t.Errorf("waived advisory + fixed blocking must converge\nstdout: %s", out)
	}
	if got := reconLen(m, "open_blocking"); got != 0 {
		t.Errorf("open_blocking = %d, want 0", got)
	}
	if got := reconLen(m, "open_advisory"); got != 0 {
		t.Errorf("open_advisory = %d, want 0 (waived)", got)
	}
	if ids := reconIDs(t, m, "waived"); len(ids) != 1 || ids[0] != "style-nit" {
		t.Errorf("waived = %v, want [style-nit]", ids)
	}
	if got := reconLen(m, "rejected_waivers"); got != 0 {
		t.Errorf("rejected_waivers = %d, want 0", got)
	}
	if got := reconStr(m, "last_verdict"); got != "approve" {
		t.Errorf("last_verdict = %q, want approve", got)
	}
	if got, ok := m["round"].(float64); !ok || got != 2 {
		t.Errorf("round = %v, want 2 (latest verdict)", m["round"])
	}
}

// TestReconcileLedger_BlockingWaiverAttemptRejected: spec guarantee 4 —
// blocking-class findings are NEVER author-waivable. The attempt is surfaced
// in rejected_waivers (with its class), the finding stays open, and an
// unknown-id waiver fails closed the same way (its class cannot be verified).
func TestReconcileLedger_BlockingWaiverAttemptRejected(t *testing.T) {
	body := gateLedgerBody("request-changes", gateHeadSHA, 1, "full", []string{
		gateFinding("off-by-one", "correctness", "issue:3", "src/x.go:42", "new", "wrong output"),
	}, nil)
	comments := "[" + gateWaiverComment("off-by-one", "yolo") + "," +
		gateWaiverComment("no-such-id", "huh") + "]"
	dir := reconcileFixture(t, "["+gateReviewJSON(gateBotActor, "CHANGES_REQUESTED", gateHeadSHA, body)+"]", comments)
	m, out, _, err := runReconcile(t, dir)
	if err != nil {
		t.Fatalf("reconcile failed: %v\nstdout: %s", err, out)
	}
	if reconBool(m, "converged") {
		t.Errorf("a rejected waiver must not converge the ledger\nstdout: %s", out)
	}
	if ids := reconIDs(t, m, "rejected_waivers"); len(ids) != 2 {
		t.Fatalf("rejected_waivers = %v, want both attempts", ids)
	}
	rw := m["rejected_waivers"].([]any)
	first := rw[0].(map[string]any)
	if first["id"] != "no-such-id" || first["class"] != "unknown" {
		t.Errorf("unknown-id waiver must be rejected with class=unknown, got %v", first)
	}
	second := rw[1].(map[string]any)
	if second["id"] != "off-by-one" || second["class"] != "correctness" {
		t.Errorf("blocking waiver must carry the finding's class, got %v", second)
	}
	if ids := reconIDs(t, m, "open_blocking"); len(ids) != 1 || ids[0] != "off-by-one" {
		t.Errorf("the blocking finding must stay open, got %v", ids)
	}
	if got := reconLen(m, "waived"); got != 0 {
		t.Errorf("waived must stay empty, got %d", got)
	}
}

// TestReconcileLedger_LatestVerdictWins: verdicts replay by round, not by
// arrival order — round 2's approve with the finding fixed overrides round 1's
// request-changes even when round 2 arrives FIRST in the fixture. round/head/
// last_verdict echo the latest verdict (what the merge gate feeds head-check).
func TestReconcileLedger_LatestVerdictWins(t *testing.T) {
	round1 := gateLedgerBody("request-changes", gateOtherSHA, 1, "full", []string{
		gateFinding("off-by-one", "correctness", "issue:3", "src/x.go:42", "new", "wrong output"),
	}, nil)
	round2 := gateLedgerBody("approve", gateHeadSHA, 2, "full", []string{
		gateFinding("off-by-one", "correctness", "issue:3", "src/x.go:42", "fixed", "wrong output"),
	}, nil)
	// Round 2 FIRST: selection must be by round, never by list order.
	reviews := "[" + gateReviewJSON(gateBotActor, "APPROVED", gateHeadSHA, round2) + "," +
		gateReviewJSON(gateBotActor, "CHANGES_REQUESTED", gateOtherSHA, round1) + "]"
	dir := reconcileFixture(t, reviews, "[]")
	m, out, _, err := runReconcile(t, dir)
	if err != nil {
		t.Fatalf("reconcile failed: %v\nstdout: %s", err, out)
	}
	if !reconBool(m, "converged") {
		t.Errorf("round 2 marked the finding fixed; ledger must converge\nstdout: %s", out)
	}
	if got := reconStr(m, "last_verdict"); got != "approve" {
		t.Errorf("last_verdict = %q, want approve (latest round)", got)
	}
	if got, ok := m["round"].(float64); !ok || got != 2 {
		t.Errorf("round = %v, want 2", m["round"])
	}
	if got := reconStr(m, "head"); got != gateHeadSHA {
		t.Errorf("head = %q, want the latest verdict's head", got)
	}
}

// TestReconcileLedger_MalformedLedgerBlockErrors: a bot body carrying a
// REVIEW-LEDGER v1 block that does not parse is a HARD stop — JSON {"error":
// ...} on stdout with a non-zero exit, never a silent shrink of the ledger.
func TestReconcileLedger_MalformedLedgerBlockErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing END marker", "REVIEW-LEDGER v1\nround: 1\nhead: " + gateHeadSHA + "\nlane: full\nverdict: approve\n"},
		{"unknown ledger key inside the block", strings.Replace(
			gateLedgerBody("approve", gateHeadSHA, 1, "full", nil, nil),
			"\ncoverage:", "\nextra-knob: 1\ncoverage:", 1)},
		{"bad class", strings.Replace(gateLedgerBody("approve", gateHeadSHA, 1, "full",
			[]string{gateFinding("x", "cosmetics", "issue:1", "a.go:1", "new", "r")}, nil), "class=cosmetics", "class=cosmetics", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := reconcileFixture(t, "["+gateReviewJSON(gateBotActor, "CHANGES_REQUESTED", gateHeadSHA, tc.body)+"]", "[]")
			_, out, stderr, err := runReconcile(t, dir)
			if err == nil {
				t.Fatalf("malformed ledger must exit non-zero\nstdout: %s\nstderr: %s", out, stderr)
			}
			var m map[string]any
			if jerr := json.Unmarshal([]byte(out), &m); jerr != nil {
				t.Fatalf("stdout must still be JSON: %v\nstdout: %s", jerr, out)
			}
			if msg, _ := m["error"].(string); msg == "" {
				t.Errorf("stdout JSON must carry a non-empty error field, got %s", out)
			}
		})
	}
}

// TestReconcileLedger_IdempotentByteIdentical: re-running on the same input
// must yield byte-identical JSON (sort_keys + id-sorted arrays) — /merge
// gates on this output and any nondeterminism would make the gate flake.
func TestReconcileLedger_IdempotentByteIdentical(t *testing.T) {
	body := gateLedgerBody("request-changes", gateHeadSHA, 1, "full", []string{
		gateFinding("null-guard", "security", "rule:7", "src/y.go:10", "new", "panic on nil"),
		gateFinding("off-by-one", "correctness", "issue:3", "src/x.go:42", "new", "wrong output"),
		gateFinding("style-nit", "style", "rule:2", "src/x.go:1", "new", "naming"),
	}, []string{"id=style-nit; reason=reviewer-side pre-waive"})
	comments := "[" + gateWaiverComment("style-nit", "author's authoritative reason") + "]"
	dir := reconcileFixture(t, "["+gateReviewJSON(gateBotActor, "CHANGES_REQUESTED", gateHeadSHA, body)+"]", comments)
	_, out1, _, err1 := runReconcile(t, dir)
	if err1 != nil {
		t.Fatalf("first reconcile failed: %v\nstdout: %s", err1, out1)
	}
	_, out2, _, err2 := runReconcile(t, dir)
	if err2 != nil {
		t.Fatalf("second reconcile failed: %v\nstdout: %s", err2, out2)
	}
	if out1 != out2 {
		t.Errorf("reconcile output must be byte-identical across runs\n--- run1:\n%s\n--- run2:\n%s", out1, out2)
	}
}

// TestReconcileLedger_WaiverByNonAuthorIgnored: only the PR author's waiver
// comments are the authoritative record — a bystander's waive must not close
// an advisory finding (admin authority is not machine-derivable).
func TestReconcileLedger_WaiverByNonAuthorIgnored(t *testing.T) {
	body := gateLedgerBody("request-changes", gateHeadSHA, 1, "full", []string{
		gateFinding("style-nit", "style", "rule:2", "src/x.go:1", "new", "naming"),
	}, nil)
	comments := "[" + gateCommentJSON("bystander", "waive style-nit: not my call but sure\n") + "]"
	dir := reconcileFixture(t, "["+gateReviewJSON(gateBotActor, "CHANGES_REQUESTED", gateHeadSHA, body)+"]", comments)
	m, out, _, err := runReconcile(t, dir)
	if err != nil {
		t.Fatalf("reconcile failed: %v\nstdout: %s", err, out)
	}
	if reconBool(m, "converged") {
		t.Errorf("a non-author waiver must not close the finding\nstdout: %s", out)
	}
	if got := reconLen(m, "open_advisory"); got != 1 {
		t.Errorf("the advisory must stay open, got open_advisory=%d", got)
	}
	if got := reconLen(m, "waived"); got != 0 {
		t.Errorf("waived must stay empty, got %d", got)
	}
	if got := reconLen(m, "rejected_waivers"); got != 0 {
		t.Errorf("a non-author attempt is ignored entirely, not rejected; rejected_waivers=%d", got)
	}
}

// TestReconcileLedger_DocsLaneMapFailsClosed: spec §"Class→blocking map" —
// docs-lane blocking = {docs-truth}; a docs-lane verdict raising a
// real-code-class finding must NOT fall out of the ledger (it blocks), or
// docs-lane merges would silently ship code bugs past review.
func TestReconcileLedger_DocsLaneMapFailsClosed(t *testing.T) {
	body := gateLedgerBody("request-changes", gateHeadSHA, 1, "docs", []string{
		gateFinding("code-bug", "correctness", "issue:1", "src/x.go:9", "new", "real code finding in a docs verdict"),
	}, nil)
	dir := reconcileFixture(t, "["+gateReviewJSON(gateBotActor, "CHANGES_REQUESTED", gateHeadSHA, body)+"]", "[]")
	m, out, _, err := runReconcile(t, dir, "--lane", "docs")
	if err != nil {
		t.Fatalf("reconcile failed: %v\nstdout: %s", err, out)
	}
	if reconBool(m, "converged") {
		t.Errorf("out-of-map class in docs lane must fail closed to blocking\nstdout: %s", out)
	}
	if ids := reconIDs(t, m, "open_blocking"); len(ids) != 1 || ids[0] != "code-bug" {
		t.Errorf("docs-lane out-of-map finding must land in open_blocking, got %v", ids)
	}
}

// TestReconcileLedger_OnlyBotMarkerCommentsCount: a comment channel verdict
// needs the check-verdict.sh marker — a bot comment carrying a stray ledger
// block but no marker (e.g. a stats post) must not enter the ledger, and a
// human-authored body with a block must not either.
func TestReconcileLedger_OnlyBotMarkerCommentsCount(t *testing.T) {
	blocked := gateLedgerBody("request-changes", gateHeadSHA, 1, "full", []string{
		gateFinding("off-by-one", "correctness", "issue:3", "src/x.go:42", "new", "wrong output"),
	}, nil)
	ledgerOnly := strings.TrimPrefix(blocked, gateMarkerBody("REQUEST_CHANGES", gateHeadSHA))
	reviews := "[]"
	comments := "[" +
		gateCommentJSON(gateBotActor, ledgerOnly) + "," +
		gateCommentJSON(gateAuthor, blocked) + "]"
	dir := reconcileFixture(t, reviews, comments)
	m, out, _, err := runReconcile(t, dir)
	if err != nil {
		t.Fatalf("reconcile failed: %v\nstdout: %s", err, out)
	}
	if !reconBool(m, "converged") {
		t.Errorf("no marker-verified verdict exists; ledger must stay empty/converged\nstdout: %s", out)
	}
	if got := reconLen(m, "open_blocking"); got != 0 {
		t.Errorf("unmarked/human ledger blocks must not count, got %d open", got)
	}
}
