{{ define "preamble_extra" }} They are summarized here for the AI workflow; the authoritative source is README-LLM.md (read it in full before making changes).{{ end }}

{{ define "tdd_extra" }}

**Full-repo validation is mandatory at the end of every task:**
```bash
go build ./...   # ALL packages must build
go test ./...    # ALL tests must pass (zero tolerance for failures, including pre-existing)
```
{{ end }}

{{ define "project_rules" }}

### 3. Type Safety (README-LLM.md Rule 2)

- Use strongly-typed structs for all data structures
- No `map[string]interface{}` for structured data
- No `interface{}` when the type is known
- Convert to typed structs immediately when parsing external data

### 4. Event System Integration (README-LLM.md Rule 4)

- ALWAYS use hook package event types from `internal/hook/events.go` — NEVER define local event types in handlers
- The hook `Dispatcher.Trigger` API uses `interface{}` for the payload — type safety is convention-enforced. Always pass typed `hook.XxxEvent` structs, never maps.
- Convert rathena-client wire event fields (`events.XxxEvent`) to hook event fields (`hook.XxxEvent`) with explicit type conversions.

### 5. Builder Pattern (README-LLM.md Rule 5)

- ALWAYS use builders (`internal/network/send/builders/`) for send packets
- NEVER manually construct packet bytes
- Builders call `session.Send(cfg.Session, cfg.Conn, session.ActionXxx, send.Xxx{...})`

### 6. rathena-client is Read-Only — rAthena is the Upstream Authority (README-LLM.md Rule 18)

- NEVER edit files under the `rathena-client` module (it is an external dependency).
- If you find a bug or gap in rathena-client: STOP, file a bug report citing rAthena source, mark the story blocked. NEVER write a workaround.
- **rAthena (`https://github.com/rathena/rathena`) is the upstream source of truth for packet structure and semantics.** When packet behaviour is in question (handler field mappings per Rule 11, diagnosing whether a bug is in goKore vs rathena-client, security review of packet handling), clone it and cross-reference: `git clone --depth 1 https://github.com/rathena/rathena.git /tmp/rathena`, then check `src/map/packets_struct.hpp` (primary), `packets.hpp`, `common/packets.hpp`, and `clif.cpp`. A bug report against rathena-client MUST cite the specific rAthena struct/field that is wrong. If goKore/rathena-client/OpenKore/memory disagrees with rAthena source, rAthena wins.

### 7. Single-Threaded Bot Instances (README-LLM.md Rule 16)

- Bot instances must be single-threaded (one GameLoop goroutine + one network read goroutine per bot)
- Bot Manager handles concurrency across the fleet
- Do not add goroutines or channels inside bot instances without explicit approval

### 8. Work Logs Are Mandatory (README-LLM.md Rule 0)

Every task MUST create a work log at `docs/07_WORK_LOG/NNNN_YYYY-MM-DD_description.md`. A task is NOT complete without a work log.
{{ end }}

{{ define "tech_debt_extra" }}
- Aggressively remove legacy code (README-LLM.md Rule 15)
{{ end }}
