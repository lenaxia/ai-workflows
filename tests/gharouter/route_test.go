// Package gharouter exercises scripts/route-command.sh, the single source of
// truth for AI-command routing in lenaxia repos.
//
// The script is bash; these tests invoke it as a subprocess with controlled
// environment variables and assert on its stdout contract:
//
//	COMMAND=/fix
//	NOTE=the bug
//	HOLD_MERGE=1
//	OUT_FILE=/tmp/...
//
// and on the assembled prompt written to OUT_FILE.
//
// Run: go test ./tests/gharouter/...
package gharouter

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// scriptPath returns the absolute path to route-command.sh relative to this
// test file (which lives at <repo>/tests/gharouter/route_test.go).
func scriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	p := filepath.Join(repoRoot, "scripts", "route-command.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("route-command.sh not found at %s: %v", p, err)
	}
	return p
}

// stubPromptsDir writes a stub .md for every prompt the router may cat, so
// prompt-assembly tests can assert ordering by content. Each stub contains a
// unique sentinel: "STUB:<name>".
func stubPromptsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	names := []string{
		"context", "core-rules", "code-change-workflow",
		"pr-review", "fix", "implement", "test", "security",
		"analyze", "explain", "triage", "design", "merge",
		"help", "issue-responder",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n+".md"), []byte("STUB:"+n+"\n"), 0o644); err != nil {
			t.Fatalf("write stub %s: %v", n, err)
		}
	}
	return dir
}

// routeResult captures the parsed stdout of a direct script invocation.
type routeResult struct {
	Command   string
	Note      string
	HoldMerge string
	OutFile   string
	Prompt    string // contents of OUT_FILE, if readable
}

// runRouter invokes route-command.sh directly (not sourced) with the given
// comment body and event context, then parses its stdout contract.
func runRouter(t *testing.T, commentBody string, onPR bool, eventName string) routeResult {
	t.Helper()
	return runRouterCtx(t, commentBody, onPR, eventName, "")
}

// runRouterCtx is the context-aware core of runRouter. headSHA, when non-empty,
// is passed to the script as PR_HEAD_SHA so review-path injection can be tested.
func runRouterCtx(t *testing.T, commentBody string, onPR bool, eventName, headSHA string) routeResult {
	t.Helper()
	script := scriptPath(t)
	promptsDir := stubPromptsDir(t)
	outFile := filepath.Join(t.TempDir(), "prompt.txt")

	prURL := ""
	if onPR {
		prURL = "https://api.github.com/repos/lenaxia/test/pulls/1"
	}
	if eventName == "" {
		eventName = "issue_comment"
	}

	cmd := exec.Command("bash", script)
	cmd.Env = []string{
		"COMMENT_BODY=" + commentBody,
		"PR_URL=" + prURL,
		"EVENT_NAME=" + eventName,
		"PR_HEAD_SHA=" + headSHA,
		"PROMPTS_DIR=" + promptsDir,
		"OUT_FILE=" + outFile,
		// Minimal PATH so bash, grep, sed, printf resolve.
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("route-command.sh failed: %v\nstderr:\n%s", err, stderr.String())
	}

	r := routeResult{OutFile: outFile}
	for _, line := range strings.Split(stdout.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "COMMAND="):
			r.Command = strings.TrimPrefix(line, "COMMAND=")
		case strings.HasPrefix(line, "NOTE="):
			r.Note = strings.TrimPrefix(line, "NOTE=")
		case strings.HasPrefix(line, "HOLD_MERGE="):
			r.HoldMerge = strings.TrimPrefix(line, "HOLD_MERGE=")
		}
	}
	if b, err := os.ReadFile(outFile); err == nil {
		r.Prompt = string(b)
	}
	return r
}

// ---------------------------------------------------------------------------
// Command detection
// ---------------------------------------------------------------------------

func TestCommandDetection_Leading(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"/implement", "/implement"},
		{"/implement add feature", "/implement"},
		{"/security", "/security"},
		{"/security check this", "/security"},
		{"/analyze", "/analyze"},
		{"/analyze the flow", "/analyze"},
		{"/review", "/review"},
		{"/review please", "/review"},
		{"/explain", "/explain"},
		{"/explain the type", "/explain"},
		{"/triage", "/triage"},
		{"/triage this issue", "/triage"},
		{"/design", "/design"},
		{"/design the api", "/design"},
		{"/merge", "/merge"},
		{"/test", "/test"},
		{"/test the foo", "/test"},
		{"/fix", "/fix"},
		{"/fix the bug", "/fix"},
		{"/help", "/help"},
		{"/ai", "/ai"},
		{"/ai do thing", "/ai"},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			r := runRouter(t, c.body, false, "")
			if r.Command != c.want {
				t.Errorf("command: got %q, want %q", r.Command, c.want)
			}
		})
	}
}

func TestCommandDetection_Inline(t *testing.T) {
	// Commands appearing mid-comment with spaces around them must still route.
	cases := []struct {
		body string
		want string
	}{
		{"hey can you /fix this", "/fix"},
		{"please /implement the feature now", "/implement"},
		{"let's /review it", "/review"},
		{"could you /analyze this for me", "/analyze"},
		{"please /merge after review", "/merge"},
		{"now /test the parser", "/test"},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			r := runRouter(t, c.body, false, "")
			if r.Command != c.want {
				t.Errorf("inline command: got %q, want %q", r.Command, c.want)
			}
		})
	}
}

func TestCommandDetection_NoFalsePositives(t *testing.T) {
	// Prefix-collision trap: /testing must NOT become /test, /fixing must NOT
	// become /fix. The router uses end-of-string-or-space anchors to prevent it.
	// These must fall through to the /ai default.
	cases := []string{
		"/testing the parser",
		"/fixing the bug",
		"/implementation details",
		"/reviewing now",
		"/analyzing",
		"/helpers",
		"/designs the schema",
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			r := runRouter(t, body, false, "")
			if r.Command != "/ai" {
				t.Errorf("prefix collision: %q routed to %q, want /ai default", body, r.Command)
			}
		})
	}
}

func TestCommandDetection_UnknownFallsToAI(t *testing.T) {
	cases := []string{
		"hello world",
		"/unknown",
		"/random command",
		"just a normal comment",
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			r := runRouter(t, body, false, "")
			if r.Command != "/ai" {
				t.Errorf("unknown: %q routed to %q, want /ai", body, r.Command)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NOTE extraction
// ---------------------------------------------------------------------------

func TestNoteExtraction(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no trailing text", "/fix", ""},
		{"simple trailing", "/fix the bug", "the bug"},
		{"trailing whitespace trimmed", "/fix   the bug   ", "the bug"},
		{"tabs trimmed", "/fix\tthe bug\t", "the bug"},
		{"inline command keeps surrounding text", "hey /fix the bug please", "the bug please"},
		{"multi word", "/implement add a cache layer for logins", "add a cache layer for logins"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runRouter(t, c.body, false, "")
			if r.Note != c.want {
				t.Errorf("NOTE: got %q, want %q", r.Note, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// --no-merge hold behavior
// ---------------------------------------------------------------------------

func TestNoMerge_Trailing_HoldsForCodeChangeCommands(t *testing.T) {
	// These four auto-merging code-change commands must honor a TRAILING
	// --no-merge by setting HOLD_MERGE=1 and stripping the token from NOTE.
	holds := []string{"/fix", "/implement", "/test", "/security"}
	for _, cmd := range holds {
		t.Run(cmd+" add thing --no-merge", func(t *testing.T) {
			r := runRouter(t, cmd+" add thing --no-merge", true, "")
			if r.HoldMerge != "1" {
				t.Errorf("HOLD_MERGE: got %q, want 1", r.HoldMerge)
			}
			if r.Note != "add thing" {
				t.Errorf("NOTE: got %q, want %q (flag stripped)", r.Note, "add thing")
			}
			if r.Command != cmd {
				t.Errorf("COMMAND: got %q, want %q", r.Command, cmd)
			}
		})
	}
}

func TestNoMerge_Trailing_NoHoldForNonCodeCommands(t *testing.T) {
	// Non-code commands never hold; flag is still stripped from NOTE.
	noHolds := []string{"/analyze", "/explain", "/triage", "/merge", "/help", "/review", "/ai"}
	for _, cmd := range noHolds {
		t.Run(cmd+" topic --no-merge", func(t *testing.T) {
			r := runRouter(t, cmd+" topic --no-merge", true, "")
			if r.HoldMerge != "0" {
				t.Errorf("HOLD_MERGE: got %q, want 0", r.HoldMerge)
			}
			if r.Note != "topic" {
				t.Errorf("NOTE: got %q, want %q", r.Note, "topic")
			}
		})
	}
}

func TestNoMerge_DesignNeverHoldsViaFlag(t *testing.T) {
	// /design holds via its own prompt, not via the --no-merge flag. The flag
	// must NOT set HOLD_MERGE for /design. This guards against over-broadening
	// the hold set.
	r := runRouter(t, "/design the api --no-merge", true, "")
	if r.HoldMerge != "0" {
		t.Errorf("HOLD_MERGE: got %q, want 0 (/design holds via prompt only)", r.HoldMerge)
	}
}

func TestNoMerge_GreedyBugRegression(t *testing.T) {
	// Regression: a mid-description "--no-merge" literal must NOT be treated
	// as the flag. This is the explicit case called out in route-command.sh.
	// The full original comment was:
	//   "/fix the --no-merge stripping is greedy"
	// It must route to /fix, NOT hold, and NOTE must retain the literal token.
	r := runRouter(t, "/fix the --no-merge stripping is greedy", true, "")
	if r.Command != "/fix" {
		t.Errorf("COMMAND: got %q, want /fix", r.Command)
	}
	if r.HoldMerge != "0" {
		t.Errorf("HOLD_MERGE: got %q, want 0 (mid-string literal must not hold)", r.HoldMerge)
	}
	wantNote := "the --no-merge stripping is greedy"
	if r.Note != wantNote {
		t.Errorf("NOTE: got %q, want %q (mid-string token must be retained)", r.Note, wantNote)
	}
}

func TestNoMerge_LeadingPositionNotFlag(t *testing.T) {
	// A leading --no-merge before the description is treated as ordinary text,
	// not as the flag (the flag is recognized only in trailing position).
	r := runRouter(t, "/implement --no-merge add cache", true, "")
	if r.Command != "/implement" {
		t.Errorf("COMMAND: got %q, want /implement", r.Command)
	}
	if r.HoldMerge != "0" {
		t.Errorf("HOLD_MERGE: got %q, want 0 (leading position is not the flag)", r.HoldMerge)
	}
	// NOTE should retain the literal token since it wasn't recognized as flag.
	if r.Note != "--no-merge add cache" {
		t.Errorf("NOTE: got %q, want %q", r.Note, "--no-merge add cache")
	}
}

func TestNoMerge_FlagWithWhitespaceVariations(t *testing.T) {
	// Trailing --no-merge with various whitespace precedes (tab, multiple
	// spaces) must still be recognized as the flag.
	cases := []string{
		"/fix the bug\t--no-merge",
		"/fix the bug   --no-merge",
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			r := runRouter(t, body, true, "")
			if r.HoldMerge != "1" {
				t.Errorf("HOLD_MERGE: got %q, want 1", r.HoldMerge)
			}
		})
	}
}

// TestTabSeparatorAccepted verifies that a command followed by a TAB (rather
// than a space) is still recognized. The original route-command.sh used
// literal-space case patterns (/cmd\ *) which required a space after the
// command token — tabs, newlines, and other whitespace did not match. This
// was inconsistent with the rest of the script (which uses [[:space:]] for
// --no-merge detection and NOTE trimming). Fixed by unifying on [[:space:]]
// in the case patterns.
func TestTabSeparatorAccepted(t *testing.T) {
	r := runRouter(t, "/fix\tthe bug", true, "")
	if r.Command != "/fix" {
		t.Errorf("tab-separated command: got %q, want /fix", r.Command)
	}
	if r.Note != "the bug" {
		t.Errorf("NOTE: got %q, want %q", r.Note, "the bug")
	}
}

// ---------------------------------------------------------------------------
// Prompt assembly
// ---------------------------------------------------------------------------

func TestPromptAssembly_OrderAndSentinels(t *testing.T) {
	// Every assembled prompt must begin with the context stub, then a
	// separator, then the core-rules stub, then a separator, then the
	// command-specific stub. This guards the canonical assembly shape.
	r := runRouter(t, "/analyze the flow", false, "")
	p := r.Prompt

	requiredOrder := []string{
		"STUB:context",
		"STUB:core-rules",
		"STUB:analyze",
	}
	idx := 0
	for _, want := range requiredOrder {
		found := strings.Index(p[idx:], want)
		if found < 0 {
			t.Errorf("prompt missing or out of order after position %d: %q\nfull:\n%s", idx, want, p)
			return
		}
		idx += found + len(want)
	}
}

func TestPromptAssembly_CodeChangeWorkflowAppended(t *testing.T) {
	// The five code-change commands must append the code-change-workflow stub
	// AFTER their command stub.
	codeChangeCmds := map[string]string{
		"/fix":       "STUB:fix",
		"/implement": "STUB:implement",
		"/test":      "STUB:test",
		"/security":  "STUB:security",
		"/design":    "STUB:design",
	}
	for body, stub := range codeChangeCmds {
		t.Run(body, func(t *testing.T) {
			r := runRouter(t, body+" something", true, "")
			iCmd := strings.Index(r.Prompt, stub)
			iWorkflow := strings.Index(r.Prompt, "STUB:code-change-workflow")
			if iCmd < 0 || iWorkflow < 0 {
				t.Fatalf("missing stubs in prompt:\n%s", r.Prompt)
			}
			if iWorkflow < iCmd {
				t.Errorf("code-change-workflow must come AFTER %s; got workflow@%d, cmd@%d", stub, iWorkflow, iCmd)
			}
		})
	}
}

func TestPromptAssembly_NonCodeCommandHasNoWorkflow(t *testing.T) {
	// /analyze, /explain, /triage, /merge, /help must NOT append the workflow.
	noWorkflow := []string{"/analyze x", "/explain x", "/triage x", "/merge", "/help"}
	for _, body := range noWorkflow {
		t.Run(body, func(t *testing.T) {
			r := runRouter(t, body, false, "")
			if strings.Contains(r.Prompt, "STUB:code-change-workflow") {
				t.Errorf("non-code command must not include workflow:\n%s", r.Prompt)
			}
		})
	}
}

func TestPromptAssembly_NoteAppendedForRelevantCommands(t *testing.T) {
	// When NOTE is present, it must appear in the assembled prompt for
	// commands that consume it.
	cases := []struct {
		body    string
		wantSub string
	}{
		{"/fix the parser bug", "the parser bug"},
		{"/implement add cache", "add cache"},
		{"/review focus on race conditions", "focus on race conditions"},
		{"/analyze the startup flow", "the startup flow"},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			r := runRouter(t, c.body, true, "")
			if !strings.Contains(r.Prompt, c.wantSub) {
				t.Errorf("prompt must contain NOTE %q:\n%s", c.wantSub, r.Prompt)
			}
		})
	}
}

func TestPromptAssembly_HoldMessageAppended(t *testing.T) {
	// When HOLD_MERGE=1, an explicit MERGE HOLD block must be appended and
	// must reference the command.
	r := runRouter(t, "/implement add thing --no-merge", true, "")
	if !strings.Contains(r.Prompt, "MERGE HOLD") {
		t.Errorf("held prompt must contain MERGE HOLD marker:\n%s", r.Prompt)
	}
	if !strings.Contains(r.Prompt, "/implement") {
		t.Errorf("hold message must reference the command:\n%s", r.Prompt)
	}
}

func TestPromptAssembly_NoHoldMessageWhenNotHeld(t *testing.T) {
	r := runRouter(t, "/fix the bug", true, "")
	if strings.Contains(r.Prompt, "MERGE HOLD") {
		t.Errorf("non-held prompt must not contain MERGE HOLD:\n%s", r.Prompt)
	}
}

// ---------------------------------------------------------------------------
// /ai context branching
// ---------------------------------------------------------------------------

func TestAI_Branching(t *testing.T) {
	t.Run("with_note_takes_freeform_path", func(t *testing.T) {
		// A trailing note overrides PR/issue context: /ai <text> is a direct ask.
		r := runRouter(t, "/ai what does session.Feed do", true, "")
		if !strings.Contains(r.Prompt, "what does session.Feed do") {
			t.Errorf("freeform /ai note must appear in prompt:\n%s", r.Prompt)
		}
		// Should NOT have appended pr-review or issue-responder stubs as the
		// primary route (note path is a printf, not a cat).
		if strings.Contains(r.Prompt, "STUB:pr-review") {
			t.Errorf("freeform /ai must not append pr-review stub:\n%s", r.Prompt)
		}
	})

	t.Run("on_PR_without_note_takes_review_path", func(t *testing.T) {
		r := runRouter(t, "/ai", true, "issue_comment")
		if !strings.Contains(r.Prompt, "STUB:pr-review") {
			t.Errorf("/ai on a PR without note must append pr-review stub:\n%s", r.Prompt)
		}
	})

	t.Run("review_comment_without_note_takes_review_path", func(t *testing.T) {
		// pull_request_review_comment events imply PR context even though
		// PR_URL extraction differs.
		r := runRouter(t, "/ai", true, "pull_request_review_comment")
		if !strings.Contains(r.Prompt, "STUB:pr-review") {
			t.Errorf("/ai on review comment must append pr-review stub:\n%s", r.Prompt)
		}
	})

	t.Run("on_issue_without_note_takes_responder_path", func(t *testing.T) {
		r := runRouter(t, "/ai", false, "issue_comment")
		if !strings.Contains(r.Prompt, "STUB:issue-responder") {
			t.Errorf("/ai on an issue without note must append issue-responder stub:\n%s", r.Prompt)
		}
	})
}

// ---------------------------------------------------------------------------
// PR_HEAD_SHA injection (which commit was reviewed)
// ---------------------------------------------------------------------------

func TestReview_HeadSHAInjectedForReviewPaths(t *testing.T) {
	// Both explicit `/review` and `/ai` re-review (on a PR, no note) must
	// surface the exact commit SHA being reviewed so the review body can
	// state which commit it covers.
	const sha = "abc123def4567890abcdef0123456789abcdef01"
	for _, body := range []string{"/review", "/ai"} {
		t.Run(body, func(t *testing.T) {
			r := runRouterCtx(t, body, true, "issue_comment", sha)
			if !strings.Contains(r.Prompt, "Commit under review") {
				t.Errorf("review prompt must label the SHA line:\n%s", r.Prompt)
			}
			if !strings.Contains(r.Prompt, sha) {
				t.Errorf("review prompt must contain head SHA %q:\n%s", sha, r.Prompt)
			}
		})
	}
}

func TestReview_HeadSHAOmittedWhenEmpty(t *testing.T) {
	// When no SHA is supplied (e.g. issue_comment without API resolution),
	// the review path must still assemble cleanly and omit the SHA line
	// rather than printing an empty backtick block.
	for _, body := range []string{"/review", "/ai"} {
		t.Run(body, func(t *testing.T) {
			r := runRouterCtx(t, body, true, "issue_comment", "")
			if strings.Contains(r.Prompt, "Commit under review") {
				t.Errorf("review prompt must omit SHA line when empty:\n%s", r.Prompt)
			}
			if strings.TrimSpace(r.Prompt) == "" {
				t.Errorf("review prompt must still be non-empty without SHA")
			}
		})
	}
}

func TestReview_HeadSHANotInjectedForNonReviewCommands(t *testing.T) {
	// A code-change command on a PR must NOT get the review SHA line; only
	// review-producing paths surface the reviewed commit.
	const sha = "deadbeef"
	for _, body := range []string{"/fix the bug", "/implement x", "/ai freeform question"} {
		t.Run(body, func(t *testing.T) {
			r := runRouterCtx(t, body, true, "issue_comment", sha)
			if strings.Contains(r.Prompt, "Commit under review") {
				t.Errorf("non-review command must not get review SHA line:\n%s", r.Prompt)
			}
		})
	}
}

func TestReview_HeadSHAInjectedForReviewCommentEvent(t *testing.T) {
	// pull_request_review_comment events imply PR context and must inject
	// the SHA on the `/ai` re-review path just like issue_comment does.
	const sha = "feedface"
	r := runRouterCtx(t, "/ai", true, "pull_request_review_comment", sha)
	if !strings.Contains(r.Prompt, sha) {
		t.Errorf("review-comment event must inject head SHA:\n%s", r.Prompt)
	}
}

// ---------------------------------------------------------------------------
// Stability: every command produces a non-empty prompt and ends cleanly.
// ---------------------------------------------------------------------------

func TestAllCommandsProduceNonEmptyPrompt(t *testing.T) {
	commands := []string{
		"/fix x", "/implement x", "/test x", "/security x",
		"/analyze x", "/explain x", "/triage x",
		"/design x", "/merge", "/help", "/ai",
	}
	sort.Strings(commands)
	for _, body := range commands {
		t.Run(body, func(t *testing.T) {
			r := runRouter(t, body, true, "")
			if strings.TrimSpace(r.Prompt) == "" {
				t.Errorf("empty prompt for %q", body)
			}
			// Sanity: every prompt has the context header.
			if !strings.HasPrefix(r.Prompt, "STUB:context") {
				t.Errorf("prompt does not start with context stub:\n%s", r.Prompt)
			}
		})
	}
}
