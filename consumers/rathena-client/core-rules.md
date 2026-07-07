{{ define "preamble_extra" }} They are summarized here for the AI workflow; the authoritative source is README-LLM.md (read it in full before making changes). rathena-client has 13 critical rules, numbered 0–12.{{ end }}

{{ define "tdd_extra" }}

Every code change must additionally include: **fuzz tests for all bit-packing functions**, and **benchmarks verifying 0 allocs/op** on decode/encode functions.

**Full-repo validation is mandatory at the end of every task:**
```bash
go build ./...                              # ALL packages must build
go test ./...                               # ALL tests must pass (zero tolerance for failures, including pre-existing)
go test -race ./...                         # race detector
grep -r "^\s*go " pkg/                      # must produce zero output (Rule 3)
go test -bench=. -benchmem ./pkg/...        # decode/encode must meet alloc targets (Rule 4)
```
{{ end }}

{{ define "assumptions_extra" }}

**rathena-client-specific:** This is doubly critical here because the semantic DB has hundreds of known errors — never trust it without GCC verification (Rules 9, 12). State every assumption with a confidence level (LOW/MEDIUM/HIGH) per Rule 10. Validate via the GCC preprocessor where packet structure is involved. Every assertion about packet structure, field names, or field types MUST cite a specific rAthena file:line or GCC preprocessor output (Rule 12).
{{ end }}

{{ define "project_rules" }}

### 3. Zero Goroutines in `pkg/` — README-LLM.md Rule 3

No `go` statement may appear anywhere in any `pkg/` file. Ever. The library owns no concurrency — `session.Feed()` is synchronous and the caller owns threading. If you need concurrency, it belongs in goKore (the consumer), not in this library.

### 4. Zero Heap Allocations in the Decode Hot Path — README-LLM.md Rule 4

`session.Feed()` must produce 0 allocs/op in steady state. Event structs are stack-allocated inside generated decode functions and passed by value to callbacks — they must not escape to the heap. If a change to a decode function causes a benchmark to show allocs, fix the generated code.

### 5. No External Runtime Dependencies — README-LLM.md Rule 5

`go.mod` must have zero `require` entries. Use only the Go standard library. Never add an external dependency. The library must be embeddable with no transitive dependency surprises.

### 6. rAthena is the Only Source of Truth — README-LLM.md Rules 6, 9

- rAthena source (`packets_struct.hpp`, `packets.hpp`, `common/packets.hpp`) is the PRIMARY and ONLY authority for packet structure, field names, field types, field order, and sizes. Use rAthena field names (e.g. `AID`, `GID`, `speed`) on wire structs.
- The semantic DB (`semantics/mappings.yaml`) is ONLY a starting point. Access it via the `gokore-semantics` MCP server ONLY — NEVER grep or edit `mappings.yaml` directly. The DB has known errors; verify every claim against rAthena source before trusting it.

**Clone rAthena before any packet-structure work.** This workflow has no pre-existing rAthena checkout:
```bash
git clone --depth 1 https://github.com/rathena/rathena.git /tmp/rathena
```
Cross-reference field names, C types, order, sizes, and `#if PACKETVER` conditionals against `/tmp/rathena/src/map/packets_struct.hpp` (primary), `packets.hpp`, and `common/packets.hpp`; confirm `clif.cpp` send/receive sites for real field usage. If the GCC preprocessor is available, run it (`g++ -E -P -DPACKETVER=… -I /tmp/rathena/src …`) to resolve conditionals to ground truth. **If anything in rathena-client, the DB, OpenKore, or your memory disagrees with rAthena source, rAthena wins.** Never ship a packet change without a cited rAthena file reference (Rule 12) — compatibility with rAthena is non-negotiable.

### 7. Type Safety in the Hot Path — README-LLM.md Rule 8

- NEVER use `interface{}`, `any`, or reflection in any decode/encode path. Use direct byte reads with offset arithmetic and typed returns.
- No `interface{}`/`any` when the type is known anywhere in the codebase.
- The public API is packet-ID agnostic: no `uint16` packet ID may appear in any exported signature — callers use semantic actions.

### 8. No Unverified Claims — README-LLM.md Rule 12

Never state that something exists without showing it (file path + source text). Never state that something does not exist without proving its absence (show the grep/GCC command and its empty output, searching ALL plausible `.hpp`/`.cpp` locations). Confident false claims are worse than admitting uncertainty.

### 9. No Destructive Git Operations — README-LLM.md Rule 2

Multiple agents may work in this repository simultaneously. NEVER run `git checkout .`, `git reset --hard`, or `git clean -fd`. Revert files one at a time with explicit confirmation. Always check `git status` first.

### 10. No Comments in Code — README-LLM.md Rule 11

Code should be self-documenting. Exceptions: **package-level doc comments** (`// Package foo ...`) are required on every package; field reads in decode functions MUST cite the rAthena field name as a comment (e.g. `// rAthena: AID`). Do NOT add inline comments like `// increment offset`.

### 11. Work Logs Are Mandatory — README-LLM.md Rule 0

Every task MUST create a work log at `docs/WORKLOG/NNNN_YYYY-MM-DD_description.md`. A task is NOT complete without a work log.
{{ end }}
