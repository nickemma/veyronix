# ADR-0001 — Modular monolith with ports and adapters

- **Status:** Accepted
- **Date:** 2026-07-20
- **Deciders:** @nickemma

## Context

Veyronix has clear internal seams — identity, authorization, projects, secrets, deployment, audit, notification — and three process types that need different subsets of them (API, worker, outbox relay). The obvious two options are a service-per-seam architecture, or a single codebase with disciplined boundaries.

The system is pre-first-deploy with one maintainer. Microservices would mean seven repositories, seven CI pipelines, seven deployment targets, network calls where function calls would do, and distributed tracing as a prerequisite for basic debugging — before a single deploy has succeeded end to end. The failure mode is well documented: a distributed monolith with all the operational cost of services and none of the independence.

But "one codebase" degrades into a ball of mud unless boundaries are enforced by something. In a service architecture the network enforces them. In a monolith, something else must.

## Decision

A **modular monolith with ports and adapters**. One repository, one module graph, several binaries built from it.

Each module under `internal/modules/<name>/` has the same shape:

```
domain/    entities, value objects, invariants — imports nothing from the project
ports/     interfaces the module needs (driven) and offers (driving)
app/       use cases — orchestration only, no SQL, no HTTP, no provider SDKs
adapters/  postgres, nats, connect, worker — implement or drive the ports
module.go  module-local wiring
```

Enforced rules:

1. Dependencies point inward. `domain` imports nothing; `app` imports `domain` and `ports`; `adapters` may import everything; only `internal/app` (the composition root) imports adapters.
2. Ports are declared by the **consumer**, not the implementer. `deployment/ports` declares `JobRepository`; `deployment/adapters/postgres` satisfies it.
3. No module reads another module's tables or repositories. Cross-module access goes through the target's inbound port, injected at the composition root.
4. Anything asynchronous crosses as a published event, not a call.

Dependency injection is manual constructor injection in `internal/app/`. No container library, no reflection, no code generation.

## Alternatives Considered

**Microservices from day one.** Rejected: pays the full distributed-systems tax before the domain boundaries have been validated by real usage. The boundaries drawn today are hypotheses; hypotheses are cheap to move inside a monolith and expensive to move across a network.

**Layered architecture (controllers / services / repositories).** Rejected: layers slice horizontally, so a change to deployment logic touches three packages and every layer accumulates unrelated code. Modules slice vertically; a change to deployment logic stays in `modules/deployment`.

**Plain package-per-feature with no port discipline.** Rejected: without the inward dependency rule, `app` code inevitably imports `pgx` and the use cases become untestable without a database. The rule is the whole value.

**A DI container (wire, fx, dig).** Rejected for now: the composition root is a few hundred lines of explicit constructor calls that any reader can follow top to bottom. Reconsider if it exceeds roughly 500 lines.

## Consequences

**Positive**

- `app` use cases test end to end with fakes: no Postgres, no NATS, no Docker.
- Swapping infrastructure touches one adapter folder and the composition root.
- If a module ever must become a service, its adapters change and its `app` package does not — the extraction is mechanical.
- Three binaries share one module graph; they differ only in which adapters get wired.

**Negative**

- More packages and more indirection than a naive layout. A trivial CRUD module still pays the five-folder tax.
- The rules are conventions until enforced. A lint rule on import direction is required, not optional (`go-arch-lint` or equivalent in CI).
- Interfaces defined by consumers mean the same concept can have two slightly different port definitions in two modules. That is intentional, and it surprises people.

## Revisit If

- A single module's deployment cadence or scaling profile diverges sharply — the worker is the likeliest candidate, and it is already a separate binary, which is most of the way there.
- The team grows past the point where one repository's merge conflicts dominate.
- Build times exceed the point where the feedback loop hurts.
