# ADR-0002 — A single provider interface, published in `sdk/`

- **Status:** Accepted
- **Date:** 2026-07-20
- **Deciders:** @nickemma

## Context

Veyronix must deploy to targets with almost nothing in common: a CDN with a REST API (Netlify), a bare machine reached over SSH, a PaaS with its own release model (Heroku), a container runtime (Docker), an orchestrator (Kubernetes), and eventually cloud-native services (ECS, Cloud Run) and model servers (vLLM, Triton).

The entire value proposition — "adding AWS is a plugin, not a rewrite" — rests on whether one abstraction can survive that variety. If the engine ever needs a `switch provider { case "netlify": ... }`, the platform has failed and every new target becomes an engine change.

There is a second question hiding inside the first: where does the interface live? Go's `internal/` rule means anything under `internal/` cannot be imported by outside code. Putting the interface there makes third-party providers impossible.

## Decision

One interface — `Deploy`, `Rollback`, `Status`, `Logs`, `HealthCheck`, `Validate`, `Name`, `Capabilities` — defined in the **public** `sdk/` package.

Contract obligations on implementers:

- `Deploy` **must** be idempotent with respect to `req.IdempotencyKey`. A repeated call with the same key must not produce a second release.
- `Validate` runs at project-save time, not deploy time, so credential and config errors surface when a human is looking at a form rather than when a release is half-applied.
- `Logs` must terminate its channel when the context is cancelled.
- `Capabilities()` must be honest. Declaring canary support that does not work is worse than declaring none.

The engine's total knowledge of any target is `ports.ProviderRegistry.Get(name) (sdk.Provider, error)`.

`sdk/conformance` ships a suite covering idempotency, error classification, log termination, and rollback correctness. A provider that passes it integrates without engine changes.

## Alternatives Considered

**Per-provider bespoke integration in the engine.** Rejected outright: it is the problem the project exists to solve.

**A plugin system with separate processes (HashiCorp go-plugin, or WASM).** Genuinely attractive for third-party safety — a crashing provider cannot take the worker with it. Rejected for V1: adds an RPC boundary, serialization of streaming logs, version negotiation, and a whole class of debugging problems, in exchange for isolation that matters mainly once untrusted third-party providers exist. Revisit at that point.

**A lowest-common-denominator interface where every provider supports every operation.** Rejected: it would force fake canary implementations and lie to the UI. `Capabilities()` is the alternative — providers declare what they support and the platform hides controls that would not work.

**Terraform providers as the abstraction.** Rejected: Terraform models desired infrastructure state, not application release lifecycle. Rollback, health-check-triggered revert, and streaming deploy logs are not natural in that model.

## Consequences

**Positive**

- Netlify is roughly 200 lines. So is SSH. The engine is untouched by either.
- Third parties can write providers against a published, versioned interface and prove correctness with the conformance suite before ever reading the engine.
- The interface being small is a forcing function — pressure to add provider-specific escapes is visible in review.

**Negative**

- `sdk/` is now a public API with compatibility obligations. Breaking it breaks strangers.
- Providers run in-process, so a panicking or leaking provider affects the worker. Mitigated with per-call timeouts, `recover` at the call boundary, and circuit breakers — not eliminated.
- Capability negotiation pushes complexity into the UI, which must render different controls per provider.

## Revisit If

- Third-party providers become common enough that in-process execution is a real security or stability problem → out-of-process plugins.
- Two or more providers need an operation that does not fit the interface, and the workaround is a config-map escape hatch. Two is a pattern; one is a special case.
