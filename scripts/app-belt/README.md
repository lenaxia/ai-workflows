# agentic-actor-coder app belt (orchestrator git identity)

Governance: pushes/merges/comments on lenaxia repos must land as
`agentic-actor-coder[bot]` — the identity `trusted_app_bots` admits in the
review chain. SSH remotes push as the deploy-key user instead; lane work uses
HTTPS remotes with this credential belt.

Platform delivers to every pod automatically:
- PEM:  `/home/sandbox/.secrets/agentic-actor-coder.*.private-key.pem`
- Env:  `AGENTIC_ACTOR_CODER_APPID`

## Install (fresh pod, ~5 min)

1. `mkdir -p /tmp/opencode/bin`
2. Copy `app-git-token` and `git-cred-app` there, `chmod +x` both.
3. `git config --global credential.helper '!/tmp/opencode/bin/git-cred-app'`
   (keep it BEFORE any other helper, e.g. gh's).
4. Smoke test: `/tmp/opencode/bin/app-git-token` prints a `ghs_` token;
   `git ls-remote https://github.com/lenaxia/LLMSafeSpaces.git HEAD` works.

`app-git-token` mints a GitHub App installation token (JWT signed with the
PEM, exchanged at the installations endpoint) and caches it for 50 minutes.
`git-cred-app` is a git credential helper that serves that token for
github.com. Commits that must display as the bot should be authored as
`agentic-actor-coder[bot] <335122809+agentic-actor-coder[bot]@users.noreply.github.com>`.

For gh CLI operations that create artifacts (PRs, comments, merges) use the
app token: `GH_TOKEN=$(/tmp/opencode/bin/app-git-token) gh ...` so they land
as the app rather than a personal token.

Lost twice to pod retirements (scripts lived in /tmp); this copy is the
durable source of truth.
