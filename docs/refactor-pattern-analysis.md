# Refactor analysis: applying universal patterns (Go)

## Objective

- Apply refactors from the guidance in `ai-oop-design-patterns` where they map directly to Go.
- Prioritize:
  - explicit dispatch tables over long branching
  - small interfaces / composable behavior
  - failure handled via explicit state/results, not process-wide panics
  - behavior preserved for normal paths

## Applied refactors (implemented)

- **Strategy (data-driven behavior)**
  - `internal/config/loader.go`
    - animation type registration and field validation now use registries/maps (`animationTypeRegistrars`, `animationEntryFieldValidators`, `animationFieldPresenters`).
  - `internal/config/loader.go`
    - hex nibble parsing uses a lookup map (`hexNibbleValues`) rather than chained conditionals.
  - `internal/matrix/scheduler.go`
    - loop/restore/retry/spec behaviors are dispatched via maps.
  - `internal/animations/layout.go`
    - layout rotations and valid-rotation checks are map-driven (`rotationTransforms`, `validRotationSet`).
  - `internal/integrations/httpapi/handlers.go`
    - restore/interrupt/action validation uses set maps.

- **State transition encapsulation**
  - `internal/app/lifecycle.go`
    - lifecycle states now map state+event to transition handlers.

- **Strategy / parser table**
  - `cmd/matrix-proxy/main.go`
    - log-level parsing moved from a small `switch` to a parser map (`logLevelParsers`) with default fallback.

- **Adapter / composition-root boundary**
  - `internal/integrations/httpapi/openapi.go` + `internal/integrations/httpapi/server.go`
    - OpenAPI JSON parsing is owned by an `openAPISpec` value created during server construction and resolved per-request with controlled error handling.
    - No longer fatal on malformed embedded JSON during package init.
    - Request path now emits a controlled HTTP 500 error response.
- **Template method / explicit orchestration**
  - `cmd/matrix-proxy/main.go`
    - `main` is reduced to a single orchestration boundary, delegating setup/run flow to `run()`.
    - Error handling is centralized through returns from a single execution path instead of duplicated `os.Exit` branches.

- **Validation simplification with polymorphic-like dispatch**
  - `internal/config/loader.go`
    - schema field validation and unknown-field handling moved into map-backed validators.
  - `internal/animations/builtin.go`
    - built-in animation factory lookup via map (`builtinAnimationGenerators`).

- **Error handling hardening**
  - `internal/animations/builtin.go`
    - removed panic from notification drawing path; rendering now returns error on invalid draw operations.
  - `internal/animations/layout.go`
    - packet packing no longer panics on an unexpected layout/frame mismatch and skips invalid pixels defensively.
  - `internal/events/bus.go` + tests
    - Removed `MustNewBus` from production code.
    - All repository tests now construct buses via `NewBus` + fatal-on-error helpers (`newTestBus`), making invalid capacity explicit instead of panicking during setup.
  - `internal/animations/registry.go`
    - Renamed `MustGet` to `GetByID` to align naming with behavior (returns an error instead of panicking) and avoid constructor-name anti-patterns.

## Why some Scala-only patterns were not translated

- Several GoF patterns in the reference repo are class/trait-oriented or rely on language features not idiomatic in Go (e.g., inheritance-heavy examples, heavy template-method hierarchies).
- The refactor kept changes scoped to practical Go equivalents and avoided speculative abstractions that were not needed by current flow shape.

## Remaining opportunities (explicitly out of scope for this pass)

- `internal/events/bus.go`
  - `MustNewBus` was removed to eliminate the panic-based construction path entirely.
  - Error handling now uses `NewBus` and `NewBusWithOptions` at construction boundaries.
- Broader architectural review for interface boundaries across packages (e.g., stronger test seam points around network clients) was not part of this refactoring batch.
