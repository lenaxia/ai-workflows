You are fixing a bug in the {{ .project_name }} repository.

**Read README-LLM.md first** — it contains 18 critical rules.

Rules:
1. Read README-LLM.md and `internal/hook/events.go` before making any changes.
2. Identify the root cause — do not fix symptoms. For packet-behaviour bugs, determine whether the bug is in goKore (handler/builder logic) or in rathena-client (wire decode) by cloning rAthena and cross-referencing the field semantics: `git clone --depth 1 https://github.com/rathena/rathena.git /tmp/rathena`, then check `src/map/packets_struct.hpp` (primary), `packets.hpp`, and `clif.cpp`. If it is a rathena-client bug, file a report citing the rAthena source and stop — never work around it (Rule 18).
3. Follow TDD (Rule 1): write a failing test that reproduces the bug, then implement the fix, then verify the test passes.
4. Include regression tests that would catch the bug if it reappears.
5. Use hook package events (`internal/hook/events.go`) — never local event types (Rule 4).
6. Use builders (`internal/network/send/builders/`) for send packets — never manual byte construction (Rule 5).
7. NEVER edit or work around rathena-client. If rathena-client has a bug, file a report and mark the story blocked (Rule 18).
8. Do not add goroutines or channels inside bot instances (Rule 16).
9. Run `go build ./...` and `go test ./...` before pushing — zero failures required (full repo, not just your code).
10. Create a work log in `docs/07_WORK_LOG/` (Rule 0).
11. If the fix touches multiple layers (handler → connector → bot), ensure integration tests cover the cross-layer behavior.
