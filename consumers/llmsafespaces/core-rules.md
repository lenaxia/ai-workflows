{{ define "project_rules" }}

### 3. SOLID

Every change must follow:

- **Single Responsibility** — every type has one reason to change
- **Open/Closed** — add behavior by adding code, not by modifying existing types
- **Liskov Substitution** — subtypes are substitutable for their base types
- **Interface Segregation** — interfaces are small (1–3 methods), shaped for the caller
- **Dependency Inversion** — high-level modules never import low-level details

### 4. Quality Assessment

Assess every code change and design decision against ALL of these dimensions. A deficiency in any one is a finding:

- **Robust** — handles failures, partial states, and adversarial inputs without corruption
- **Reliable** — deterministic, repeatable, race-free, no flaky behavior
- **Maintainable** — clear naming, small functions, obvious data flow; a junior engineer can read it
- **Performant** — no unnecessary allocations, no O(n²) on hot paths, measured before claimed
- **Secure** — input validated, outputs sanitized, secrets never logged, least-privilege
- **Scalable** — no hidden bottlenecks, horizontal where needed, bounded resource usage
- **Idiomatic** — follows language conventions and surrounding codebase patterns
- **Right-Sized Complexity** — exactly as complex as needed, no more (over-engineered) no less (under-engineered). Every abstraction earns its keep with ≥2 implementations or clear imminent need

### 5. Type Safety

- Define strongly-typed structs for all data structures
- No `map[string]interface{}` for structured data
- No `interface{}` when the type is known
- Convert to typed structs immediately when parsing external data

### 6. Explicit Over Implicit

- Explicit error handling — no swallowed errors
- No magic or hidden behavior
- No comments unless strictly necessary and timeless
{{ end }}
