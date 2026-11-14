## Code Change Workflow (MANDATORY)

Every code change MUST follow this review-iterate-approve cycle without exception:

1. **Read README-LLM.md** — all 18 critical rules apply. Read `internal/hook/events.go` and relevant handler/builder code before implementing. If the change touches packet field mappings or behaviour, clone rAthena to confirm field semantics and to determine whether any discrepancy is a goKore bug or a rathena-client bug: `git clone --depth 1 https://github.com/rathena/rathena.git /tmp/rathena` (cross-reference `src/map/packets_struct.hpp`, `packets.hpp`, `clif.cpp`). If it is a rathena-client bug, file a report citing rAthena source and stop — never work around it (Rule 18).
2. **Branch:** Create a feature branch (`feat/`, `fix/`, `test/`, or `security/` prefix). Never commit to main.
3. **TDD:** Write tests first. Run them — they must fail. Write minimal code to pass. Run them — they must pass. Refactor.
4. **PR:** Open a pull request with a clear description. Reference the triggering issue or comment.
5. **Wait for review:** The automated PR review triggers on every PR open and push. Wait for it to complete before proceeding.
6. **Address feedback:** Read every finding. Each round, resolve EVERY open finding: fix it, or — for advisory classes only (`docs-truth`, `style`) — post a PR comment containing a line `waive <finding-id>: <reason>` and the reviewer will close it. Findings of the merge-gating classes (`correctness`, `robustness`, `security`, `test-integrity`) cannot be waived — fix them or push back with evidence on the PR thread (escalate; don't stonewall, and don't reword a finding to dodge its waiver id). Push to the same branch — this triggers automatic re-review.
7. **Iterate:** Repeat steps 5–6 until the review ledger converges — reviewer approve plus every open finding fixed or waived. `scripts/review-gate/reconcile-ledger.sh` computes convergence from the verdict bodies and waiver comments; it is the gate, not a round count or a vibe.
8. **Merge:** After the ledger converges only — merge with squash method, **unless this run was invoked with `--no-merge`** (see Hold below) or it is a `/design` run (which always holds).
9. **Work log:** Create a work log in `docs/07_WORK_LOG/NNNN_YYYY-MM-DD_description.md` (Rule 0 — mandatory).
10. **Report:** Post a comment on the original issue/PR confirming completion with a summary of changes.

**Merge control (`--no-merge` and `/merge`):**
- By default `/fix`, `/implement`, `/test`, and `/security` auto-merge after the review ledger converges (step 8).
- Append `--no-merge` to any of those commands to hold the merge: the run iterates to convergence but does NOT merge — it stops and waits for an explicit `/merge`.
- `/design` **always** holds — design docs never auto-merge.
- `/merge` is the explicit finalize command: it verifies the latest review is APPROVE, the review ledger has converged (`scripts/review-gate/reconcile-ledger.sh`, refuse unless `converged=true`), the verdict head still matches the PR head (`scripts/review-state.sh head-check`), and required CI is green — then squash-merges and deletes the branch.

**Hard rules:**
- NEVER merge before the review ledger converges — approval alone is not the gate, convergence is (a stale approval over open findings does not merge)
- Waivers (`waive <id>: <reason>` comments) close advisory-class findings (`docs-truth`, `style`) only — never attempt to waive a `correctness`/`robustness`/`security`/`test-integrity` finding; fix it or escalate with evidence
- NEVER dismiss review findings — fix them or document with evidence why they are false alarms
- NEVER commit directly to main
- **Full-repo validation required:** `go build ./...` must exit 0, `go test ./...` must show zero FAIL lines — fix pre-existing failures too (zero tolerance per Rule 1)
- If the review cycle exceeds 3 iterations, step back and reassess the approach — something is wrong
