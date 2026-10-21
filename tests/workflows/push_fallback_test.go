// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// This file is the EXECUTABLE half of the #50 PAT-fallback lock (textual
// half: TestDogfoodBumpHasPatFallback in workflow_invariants_test.go).
//
// propagate.yml's dogfood-bump job retries its branch push with
// AI_WORKFLOWS_PAT when the app-token push is refused with the
// workflows-permission 403. The first implementation of that retry was a
// functional no-op: it installed a SECOND global url.<base>.insteadOf
// rewrite, git's equal-length tie-breaking resolved pushes through the
// still-installed FIRST (app-token) rewrite, and the retry failed with the
// identical 403. A string-presence test cannot catch that — this test
// reproduces the credential hand-off with real git repositories on the
// file:// transport and asserts the second push authenticates against (lands
// in) the SECOND remote.
//
// Run: go test ./tests/workflows/...
package workflows

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gitRun runs git in dir with an isolated HOME (so --global config in the
// negative demo cannot touch the developer's gitconfig).
func gitRun(t *testing.T, dir, home string, args ...string) error {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// fallbackFixture builds: two bare repos (first rejects every push via a
// pre-receive hook — standing in for the workflows-permission 403; second
// accepts), and a work repo with one commit whose origin starts as a
// synthetic github URL (never contacted — every push goes through a
// credential hand-off that rewrites it).
func fallbackFixture(t *testing.T) (first, second, work, home string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	home = filepath.Join(tmp, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}

	first = filepath.Join(tmp, "first.git")
	second = filepath.Join(tmp, "second.git")
	for _, bare := range []string{first, second} {
		if err := gitRun(t, tmp, home, "init", "--bare", "-q", bare); err != nil {
			t.Fatalf("git init --bare %s: %v", bare, err)
		}
	}
	hook := filepath.Join(first, "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'refusing to allow a GitHub App to create or update workflow without `workflows' permission' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	work = filepath.Join(tmp, "work")
	if err := gitRun(t, tmp, home, "init", "-q", "-b", "main", work); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "t"},
		{"config", "user.email", "t@example.com"},
		{"commit", "--allow-empty", "-m", "bump"},
		{"checkout", "-q", "-b", "sync/dogfood-pins-test"},
		{"remote", "add", "origin", "https://github.com/demo/demo.git"},
	} {
		if err := gitRun(t, work, home, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return first, second, work, home
}

// pushAsSetURL is the CORRECTED hand-off exactly as propagate.yml's push_as
// does it: swap origin's URL to the credential-bearing one, then push by
// remote name.
func pushAsSetURL(t *testing.T, work, home, url string) error {
	t.Helper()
	if err := gitRun(t, work, home, "remote", "set-url", "origin", url); err != nil {
		t.Fatalf("set-url: %v", err)
	}
	return gitRun(t, work, home, "push", "--force-with-lease", "origin", "sync/dogfood-pins-test")
}

// pushAsInsteadOf is the ORIGINAL broken hand-off: a global insteadOf
// rewrite per attempt (the second --replace-all APPENDS a section because
// the bases differ by embedded credential). The insteadOf value is the FULL
// remote URL so the rewrite routes to a real repository and git's
// equal-length tie-break (both sections rewrite the identical URL) resolves
// to the FIRST section in config order.
func pushAsInsteadOf(t *testing.T, work, home, base string) error {
	t.Helper()
	if err := gitRun(t, work, home, "config", "--global", "--replace-all",
		"url."+base+".insteadOf", "https://github.com/demo/demo.git"); err != nil {
		t.Fatalf("config insteadOf: %v", err)
	}
	return gitRun(t, work, home, "-c", "http.https://github.com/.extraheader=",
		"push", "--force-with-lease", "origin", "sync/dogfood-pins-test")
}

// TestDogfoodPushFallbackAuthenticatesWithPat: after the first (app-token)
// push is refused, the retry must land in the SECOND remote — proving the
// credential hand-off actually switched identities.
func TestDogfoodPushFallbackAuthenticatesWithPat(t *testing.T) {
	first, second, work, home := fallbackFixture(t)

	// Attempt 1 (app token) is refused — the hook plays the 403.
	if err := pushAsSetURL(t, work, home, "file://"+first); err == nil {
		t.Fatal("first push unexpectedly succeeded — fixture does not reproduce a refusal")
	}

	// Fallback (PAT) must land in the SECOND remote.
	if err := pushAsSetURL(t, work, home, "file://"+second); err != nil {
		t.Fatalf("PAT retry failed: %v — the credential hand-off did not take effect", err)
	}
	assertLandedIn(t, work, second, first)
}

// TestDogfoodPushInsteadOfFallbackWasBroken is the negative demo locking WHY
// insteadOf is forbidden: with the original mechanism, the retry still lands
// in (is refused by) the FIRST remote, reproducing the round-1 no-op bug.
func TestDogfoodPushInsteadOfFallbackWasBroken(t *testing.T) {
	first, second, work, home := fallbackFixture(t)

	// "App token" rewrite + refused push.
	if err := pushAsInsteadOf(t, work, home, "file://"+first); err == nil {
		t.Fatal("first insteadOf push unexpectedly succeeded — fixture does not reproduce a refusal")
	}
	// "PAT" rewrite: APPENDS a second insteadOf section (bases differ).
	// Equal-length insteadOf tie-breaking resolves the identical remote URL
	// to the FIRST rewrite in config order, so this push must ALSO be
	// refused by first — the exact no-op this repo shipped in round 1.
	if err := pushAsInsteadOf(t, work, home, "file://"+second); err == nil {
		t.Fatalf("insteadOf retry landed somewhere other than the first rewrite — the tie-break premise of this test is wrong; investigate before trusting the fallback test")
	}
	// Nothing reached the second remote.
	if err := exec.Command("git", "--git-dir", second, "rev-parse", "--verify",
		"refs/heads/sync/dogfood-pins-test").Run(); err == nil {
		t.Error("second remote has commits — tie-breaking did not route through the first rewrite; premise broken")
	}

	// Harness sanity (per review round 2): swapping the insertion order must
	// flip which remote receives the first push — otherwise the fixture does
	// not discriminate config order and locks nothing.
	first2, second2, work2, home2 := fallbackFixture(t)
	if err := pushAsInsteadOf(t, work2, home2, "file://"+second2); err != nil {
		t.Fatalf("swap sanity: first push against second.git unexpectedly refused — fixture does not discriminate config order")
	}
	if err := exec.Command("git", "--git-dir", second2, "rev-parse", "--verify",
		"refs/heads/sync/dogfood-pins-test").Run(); err != nil {
		t.Error("swap sanity: first push did not land in second.git — fixture does not discriminate config order")
	}
	_ = first2
}

// assertLandedIn verifies work's HEAD == target's HEAD and the refused repo
// stayed empty.
func assertLandedIn(t *testing.T, work, target, refused string) {
	t.Helper()
	local := exec.Command("git", "-C", work, "rev-parse", "HEAD")
	local.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := local.Output()
	if err != nil {
		t.Fatalf("rev-parse work HEAD: %v", err)
	}
	want := strings.TrimSpace(string(out))

	got := exec.Command("git", "--git-dir", target, "rev-parse", "--verify", "refs/heads/sync/dogfood-pins-test")
	got.Env = []string{"PATH=" + os.Getenv("PATH")}
	gotOut, err := got.Output()
	if err != nil {
		t.Fatalf("retry did not land in the second remote (rev-parse failed): %v", err)
	}
	if strings.TrimSpace(string(gotOut)) != want {
		t.Errorf("second remote HEAD %s != work HEAD %s", strings.TrimSpace(string(gotOut)), want)
	}

	stale := exec.Command("git", "--git-dir", refused, "rev-parse", "--verify", "refs/heads/sync/dogfood-pins-test")
	stale.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := stale.Run(); err == nil {
		t.Error("the REFUSED remote has commits — the retry (or first push) leaked through the wrong credential")
	}
}

// TestDogfoodPushAuthorizationFollowsSetUrlUnderPersistedExtraheader
// reproduces the job's ACTUAL credential environment (review round 2's
// blocking defect): a checkout-persisted
// http.<origin>/.extraheader Authorization (GITHUB_TOKEN) plus
// credential-bearing remote URLs, over a real http transport (extraheaders
// do not apply to file:// — which is why the round-1 fixture was green
// while the workflow failed). The fixed push_as clears the header per push
// (git -c http.<origin>/.extraheader=); the Authorization on the wire must
// then follow the URL credential — APP token on attempt 1, PAT on the
// retry. Before the fix, both requests carry the persisted GITHUB_TOKEN.
func TestDogfoodPushAuthorizationFollowsSetUrlUnderPersistedExtraheader(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isReceive := strings.Contains(r.URL.Path, "git-receive-pack") || strings.Contains(r.URL.RawQuery, "git-receive-pack")
		auth := r.Header.Get("Authorization")
		// Mirror GitHub's actual dance: unauthenticated requests get a
		// Basic challenge (401); AUTHENTICATED receive-pack requests get
		// the #50 workflows-permission refusal (403 — never a re-challenge,
		// which is exactly why a superseding extraheader hides the URL
		// credential: the custom Authorization header satisfies the
		// challenge check and curl never offers the URL creds).
		if isReceive && auth != "" {
			mu.Lock()
			got = append(got, auth)
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("refusing to allow a GitHub App to create or update workflow `.github/workflows/x.yml` without `workflows` permission\n"))
			return
		}
		if isReceive {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	// httptest binds 127.0.0.1, but the literal IP breaks git's dotted
	// config-key parsing for http.<url>.extraheader ("invalid key" — the
	// IPv4 dots); localhost resolves identically and parses.
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	originBase := "localhost:" + port

	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := gitRun(t, tmp, home, "init", "-q", "-b", "main", work); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "t"},
		{"config", "user.email", "t@example.com"},
		{"commit", "--allow-empty", "-m", "bump"},
		{"checkout", "-q", "-b", "sync/dogfood-pins-test"},
	} {
		if err := gitRun(t, work, home, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := gitRun(t, work, home, "remote", "add", "origin",
		"http://"+originBase+"/demo/demo.git"); err != nil {
		t.Fatal(err)
	}
	// The checkout-persisted credential: what actions/checkout v6 writes
	// for the origin (AUTHORIZATION: basic base64("x-access-token:"+token)).
	persisted := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString(
		[]byte("x-access-token:GITHUB-TOKEN-SIM"))
	if err := gitRun(t, work, home, "config", "--local",
		"http.http://"+originBase+"/.extraheader", persisted); err != nil {
		t.Fatal(err)
	}

	// The FIXED push_as, mirrored: set-url per attempt + clear the
	// extraheader per push (empty -c value). Without the clear, the
	// recorded Authorization stays GITHUB-TOKEN-SIM on BOTH pushes.
	pushAs := func(token string) error {
		t.Helper()
		if err := gitRun(t, work, home, "remote", "set-url", "origin",
			"http://x-access-token:"+token+"@"+originBase+"/demo/demo.git"); err != nil {
			t.Fatalf("set-url: %v", err)
		}
		return gitRun(t, work, home,
			"-c", "http.http://"+originBase+"/.extraheader=",
			"push", "--force-with-lease", "origin", "sync/dogfood-pins-test")
	}

	if err := pushAs("APP-TOKEN-SIM"); err == nil {
		t.Fatal("first push unexpectedly succeeded — server did not reproduce the 403")
	}
	if err := pushAs("PAT-SIM"); err == nil {
		t.Fatal("retry push unexpectedly succeeded — server did not reproduce the 403")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("expected >=2 recorded Authorization headers (attempt + retry), got %d: %q", len(got), got)
	}
	for i, want := range []string{"APP-TOKEN-SIM", "PAT-SIM"} {
		b64cred := got[i]
		if i := strings.IndexAny(b64cred, " "); i >= 0 {
			b64cred = b64cred[i+1:]
		} else {
			t.Errorf("request %d: Authorization not Basic: %q", i, got[i])
			continue
		}
		dec, err := base64.StdEncoding.DecodeString(b64cred)
		if err != nil {
			t.Errorf("request %d: Authorization not decodable Basic: %q", i, got[i])
			continue
		}
		if !strings.Contains(string(dec), "x-access-token:"+want) {
			t.Errorf("request %d authenticated as %q, want the URL credential x-access-token:%s — the checkout-persisted extraheader is superseding the URL credential (clear it per push).", i, string(dec), want)
		}
	}
}
