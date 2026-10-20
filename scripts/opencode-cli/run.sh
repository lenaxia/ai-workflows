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

OC_RELEASE="opencode-v1.19.4-aiwf.1"

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

if [ ! -x "$OC_BIN" ]; then
  mkdir -p "$OC_DIR"
  echo "Installing self-hosted opencode ${OC_RELEASE} (${OC_TARGET})..."
  curl -fsSL -o /tmp/opencode.tar.gz \
    "https://github.com/lenaxia/ai-workflows/releases/download/${OC_RELEASE}/opencode-${OC_TARGET}.tar.gz"
  tar -xzf /tmp/opencode.tar.gz -C "$OC_DIR"
fi

"$OC_BIN" --version
exec "$OC_BIN" github run
