#!/usr/bin/env bash
# run.sh — install the self-hosted (patched) opencode CLI and exec the GitHub
# runner entrypoint (`opencode github run`), replacing the
# anomalyco/opencode/github@<sha> composite action the reusable workflows
# used to consume.
#
# Why self-hosted: the upstream composite installs the released opencode CLI
# whose LIVE github handler (packages/opencode/src/cli/cmd/github.handler.ts)
# refuses app-bot actors unconditionally — assertPermissions() calls
# getCollaboratorPermissionLevel, which structurally returns "none" for GitHub
# App bots (they are not collaborators), so no PR/comment authored by a
# trusted app bot can trigger the AI workflows no matter what the app's
# installation holds (#56). The TOKEN env bypass that the legacy node
# entrypoint honored does not exist in the live path. This repo builds the
# pinned upstream ref + scripts/opencode-cli/0001-*.patch (which adds the
# bypass) into release assets; the OC_RELEASE literal below pins the exact
# build. scripts/trusted-bot-auth.sh exports TOKEN only for actors on the
# caller's trusted_app_bots allowlist — the patched CLI honors it by skipping
# ONLY the collaborator assert (API auth is unchanged).
#
# Semantics mirror the composite's inner "Run opencode" step: same entrypoint,
# same env contract (MODEL/AGENT/SHARE/PROMPT/USE_GITHUB_TOKEN/MENTIONS/
# VARIANT/OIDC_BASE_URL are set by the calling step's env: block). GITHUB_ENV
# exports (OPENCODE_PROMPT, trusted-bot TOKEN) cross into this script exactly
# as they crossed the composite boundary.
#
# Bump procedure: dispatch the "Build OpenCode CLI (patched)" workflow with a
# new release_tag, then update OC_RELEASE below (and the guard test
# TestReusableWorkflowsSelfHostOpenCode pins) in the same PR.

set -euo pipefail

# Injectable for tests (tests/gharouter/runsh_test.go); the default is THE
# pin — bump it in the same PR that rebuilds the release.
OC_RELEASE="${OC_RELEASE:-opencode-v1.19.4-aiwf.1}"

case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)  OC_TARGET="linux-x64"   ;;
  Linux/aarch64) OC_TARGET="linux-arm64" ;;
  *)
    echo "::error::self-hosted opencode CLI has no build for $(uname -s)/$(uname -m). Rebuild via the 'Build OpenCode CLI (patched)' workflow with a runner for this platform."
    exit 1
    ;;
esac

OC_DIR="$HOME/.opencode/bin"
OC_BIN="$OC_DIR/opencode"

# Install if missing OR stale. Presence alone is not enough: self-hosted
# runners (gokore) persist $HOME across jobs, and the replaced composite
# re-keyed its cache on the version — without this check a bumped OC_RELEASE
# would silently keep executing the old binary on those runners, defeating
# the documented bump procedure. `opencode --version` reports the
# OPENCODE_VERSION the build was dispatched with, i.e. the release tag.
need_install=0
if [ ! -x "$OC_BIN" ]; then
  need_install=1
elif [ "$("$OC_BIN" --version 2>/dev/null || true)" != "$OC_RELEASE" ]; then
  echo "::warning::self-hosted opencode at $OC_BIN is stale (expected $OC_RELEASE) — reinstalling."
  need_install=1
fi

if [ "$need_install" = "1" ]; then
  mkdir -p "$OC_DIR"
  echo "Installing self-hosted opencode ${OC_RELEASE} (${OC_TARGET})..."
  curl -fsSL -o /tmp/opencode.tar.gz \
    "https://github.com/lenaxia/ai-workflows/releases/download/${OC_RELEASE}/opencode-${OC_TARGET}.tar.gz"
  tar -xzf /tmp/opencode.tar.gz -C "$OC_DIR"
fi

# Post-install contract: the executed binary MUST report OC_RELEASE. A
# wrong/clobbered/stale release asset otherwise runs silently — the drift
# check above establishes the contract; this enforces it on the only path
# that was still unchecked (fresh install).
if [ "$("$OC_BIN" --version 2>/dev/null || true)" != "$OC_RELEASE" ]; then
  echo "::error::self-hosted opencode at $OC_BIN reports '$("$OC_BIN" --version 2>/dev/null || echo nothing)' but OC_RELEASE is ${OC_RELEASE} — the release asset is wrong for this pin. Rebuild via the 'Build OpenCode CLI (patched)' workflow."
  exit 1
fi

"$OC_BIN" --version
exec "$OC_BIN" github run
