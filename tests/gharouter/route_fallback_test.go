// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Fallback-directive coverage for scripts/route-command.sh: the /review and
// /ai-on-PR prompt paths must carry the verdict-delivery contract (owner
// ruling ai-workflows#6209846676, issue #46): official review first; when
// GitHub refuses the review event itself, the COMPLETE review goes out as a
// PR comment carrying the machine-greppable marker
// <!-- ai-workflows-verdict: APPROVE|REQUEST_CHANGES sha=<sha> -->.
// Without the directive in the command path, a /review-triggered reviewer
// facing the approve-block submits nothing and no gate can recover it.
package gharouter

import (
	"strings"
	"testing"
)

const fallbackMarker = "ai-workflows-verdict:"

func TestRouteReviewOnPRAppendsFallbackDirective(t *testing.T) {
	res := runRouterCtx(t, "/review", true, "issue_comment", "abcdef0123456789abcdef0123456789abcdef01")
	if !strings.Contains(res.Prompt, fallbackMarker) {
		t.Errorf("/review on a PR must append the verdict-delivery fallback directive (marker %q), got prompt:\n%s", fallbackMarker, res.Prompt)
	}
	if !strings.Contains(res.Prompt, "422") {
		t.Errorf("fallback directive must name the 422 refusal class so the reviewer recognizes it, got:\n%s", res.Prompt)
	}
}

func TestRouteAiOnPRAppendsFallbackDirective(t *testing.T) {
	res := runRouterCtx(t, "/ai", true, "issue_comment", "abcdef0123456789abcdef0123456789abcdef01")
	if !strings.Contains(res.Prompt, fallbackMarker) {
		t.Errorf("/ai re-review on a PR must append the fallback directive, got prompt tail:\n%s", tail(res.Prompt, 600))
	}
}

func TestRouteFixDoesNotCarryReviewDirective(t *testing.T) {
	res := runRouter(t, "/fix the crash", false, "issue_comment")
	// /fix is a code-change command; its own flow ends with a review by the
	// pull_request workflow's prompt, not this one. The directive here would
	// be harmless but signals drift — assert the review contract rides only
	// the review-shaped paths.
	if strings.Contains(res.Prompt, fallbackMarker) {
		t.Errorf("/fix prompt must not carry the review fallback directive (review-shaped paths only), got:\n%s", tail(res.Prompt, 600))
	}
}

func TestRouteReviewOnIssueHasNoDirective(t *testing.T) {
	res := runRouter(t, "/review", false, "issue_comment")
	if strings.Contains(res.Prompt, fallbackMarker) {
		t.Errorf("/review on a plain issue has no PR verdict to deliver — directive must be absent, got:\n%s", tail(res.Prompt, 600))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
