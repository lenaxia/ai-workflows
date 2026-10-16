// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gharouter also exercises scripts/author-permission.sh — the
// pre-flight that decides, BEFORE the LLM runs, whether the opencode CLI's
// unconditional assertPermissions() will accept the PR author (the
// pull_request event's actor). Bot accounts structurally return permission
// 'none' from the collaborator check (apps are not collaborators), so
// running the LLM for them is a guaranteed ~23s death — twice, with the
// bounded retry — before the verify gate fails with a bare "no verdict
// delivered".
//
// OWNER RULING PIN (ai-workflows#6209918061 point 3): the pre-flight MUST
// read the SAME permission source as the CLI's assertPermissions —
// GET /repos/{owner}/{repo}/collaborators/{username}/permission
// (octokit repos.getCollaboratorPermissionLevel), pass-set exactly
// admin|write. A pre-flight that consults any other source (association,
// a different endpoint, a widened pass-set) could allow a run the CLI then
// refuses — burning two doomed LLM attempts — or block a run the CLI would
// have accepted. These tests make that drift impossible to reintroduce.
package gharouter

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// authorPermScriptPath resolves scripts/author-permission.sh relative to the repo.
func authorPermScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Join(filepath.Dir(file), "..", ".."), "scripts", "author-permission.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("author-permission.sh not found at %s: %v", p, err)
	}
	return p
}

// permMockDir builds a mock gh that serves {"permission": <perm>} for the
// collaborators/permission endpoint and RECORDS every requested URL
// (percent-decoded) to urls.txt. exitErr true makes the mock exit 1 on the
// permission call (API failure fixture).
func permMockDir(t *testing.T, perm string, exitErr bool) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> ` + filepath.Join(dir, "urls.txt") + `
case "$*" in
  *"collaborators/"*"/permission"*)
    if [ "` + boolStr(exitErr) + `" = "yes" ]; then
      echo "gh: api error 502" >&2
      exit 1
    fi
    # Apply the requested --jq filter like real gh does.
    jqfilter=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--jq" ]; then
        jqfilter="$arg"
      fi
      prev="$arg"
    done
    if [ -n "$jqfilter" ]; then
      echo "{\"permission\":\"` + perm + `\"}" | jq -r "$jqfilter"
    else
      echo "{\"permission\":\"` + perm + `\"}"
    fi
    ;;
  *) echo '{}' ;;
esac
`
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// runAuthorPerm runs the script with the mock on PATH and returns
// (combined output, author_allowed value from GITHUB_OUTPUT).
func runAuthorPerm(t *testing.T, dir string) (string, string) {
	t.Helper()
	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "github_output")
	cmd := exec.Command(authorPermScriptPath(t))
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REPOSITORY=lenaxia/test",
		"PR_AUTHOR=someuser",
		"GH_TOKEN=faketoken",
		"GITHUB_OUTPUT="+outFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script must exit 0 (decision lives in outputs): %v\noutput:\n%s", err, out)
	}
	allowed := ""
	if b, rerr := os.ReadFile(outFile); rerr == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "author_allowed=") {
				allowed = strings.TrimPrefix(line, "author_allowed=")
			}
		}
	}
	return string(out), allowed
}

func recordedURLs(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "urls.txt"))
	if err != nil {
		t.Fatal("mock gh recorded no URLs — the script never queried a permission source")
	}
	var urls []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		urls = append(urls, strings.ReplaceAll(strings.ReplaceAll(l, "%5B", "["), "%5D", "]"))
	}
	return urls
}

// SAME-SOURCE PIN (ruling point 3): the pre-flight must call exactly the
// REST endpoint backing octokit's getCollaboratorPermissionLevel — the one
// assertPermissions consults in the pinned CLI.
func TestAuthorPermission_SameSourceAsCLIAssert(t *testing.T) {
	dir := permMockDir(t, "write", false)
	_, allowed := runAuthorPerm(t, dir)
	if allowed != "true" {
		t.Fatalf("write must be allowed, got %q", allowed)
	}
	re := regexp.MustCompile(`^api repos/[^/]+/[^/]+/collaborators/[^/]+/permission( |$)`)
	for _, u := range recordedURLs(t, dir) {
		if !re.MatchString(u) {
			t.Errorf("pre-flight must query repos/{o}/{r}/collaborators/{user}/permission (the CLI assertPermissions source), got %q", u)
		}
	}
}

// Static drift pin: the script source itself must contain the endpoint
// path and the exact admin|write pass-set — editing the script to consult
// any other source fails here even before runtime.
func TestAuthorPermission_StaticSameSourcePin(t *testing.T) {
	b, err := os.ReadFile(authorPermScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, "/collaborators/") || !strings.Contains(src, "/permission") {
		t.Errorf("script must query the collaborators/permission endpoint (CLI assertPermissions source)")
	}
	if !strings.Contains(src, `"admin"`) || !strings.Contains(src, `"write"`) {
		t.Errorf("script's pass-set must be exactly admin|write, mirroring github.ts assertPermissions")
	}
	if strings.Contains(src, "author_association") {
		t.Errorf("script must not consult author_association — a different source than the CLI assert invites the pre-flight-allows/CLI-refuses mismatch")
	}
}

func TestAuthorPermission_AdminAndWriteAllowed(t *testing.T) {
	for _, perm := range []string{"admin", "write"} {
		dir := permMockDir(t, perm, false)
		out, allowed := runAuthorPerm(t, dir)
		if allowed != "true" {
			t.Errorf("permission %q must yield author_allowed=true, got %q\noutput:\n%s", perm, allowed, out)
		}
	}
}

func TestAuthorPermission_ReadNoneBlocked(t *testing.T) {
	for _, perm := range []string{"read", "none"} {
		dir := permMockDir(t, perm, false)
		out, allowed := runAuthorPerm(t, dir)
		if allowed != "false" {
			t.Errorf("permission %q must yield author_allowed=false (CLI assert will refuse), got %q\noutput:\n%s", perm, allowed, out)
		}
	}
}

// FAIL-OPEN PIN (the mismatch guard): when the permission source itself
// fails (transient 5xx), the pre-flight must ALLOW the run — the CLI's own
// assertPermissions remains the enforcement, so a flaky pre-flight can
// never skip (false-green) a review that would have run, and the workflow's
// tolerated-failure + verify tail handles the CLI refusal loudly.
func TestAuthorPermission_FailsOpenOnAPIError(t *testing.T) {
	dir := permMockDir(t, "", true)
	out, allowed := runAuthorPerm(t, dir)
	if allowed != "true" {
		t.Errorf("API failure must fail OPEN (author_allowed=true, CLI assert decides), got %q\noutput:\n%s", allowed, out)
	}
	if !strings.Contains(out, "fail") && !strings.Contains(out, "WARN") {
		t.Errorf("fail-open must be loud (WARN line in output), got:\n%s", out)
	}
}

// Bot logins contain [bot] — the URL must arrive percent-encoded (or safe)
// so the API resolves the right account; the decoded recorded URL must
// carry the exact login.
func TestAuthorPermission_EncodesBotLogin(t *testing.T) {
	dir := permMockDir(t, "none", false)
	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "github_output")
	cmd := exec.Command(authorPermScriptPath(t))
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REPOSITORY=lenaxia/test",
		"PR_AUTHOR=agentic-actor-coder[bot]",
		"GH_TOKEN=faketoken",
		"GITHUB_OUTPUT="+outFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script must exit 0: %v\n%s", err, out)
	}
	urls := recordedURLs(t, dir)
	found := false
	for _, u := range urls {
		if strings.Contains(u, "collaborators/agentic-actor-coder[bot]/permission") {
			found = true
		}
	}
	if !found {
		t.Errorf("bot login must reach the endpoint intact, recorded: %v", urls)
	}
}
