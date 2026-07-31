# Architecture Decision Records

Every significant decision in Veyronix is recorded here, along with the alternatives that lost and the reason they lost. The point is not ceremony. It is that in eighteen months someone — possibly the author — will ask "why is it like this?", and the honest answer should not be "I don't remember."

## Format

Each ADR uses the same sections: **Context**, **Decision**, **Alternatives Considered**, **Consequences**, **Revisit If**. The last one matters most: a decision without a stated invalidation condition is a belief, not an engineering choice.

## Status values

| Status | Meaning |
|---|---|
| Proposed | Written, not yet committed to |
| Accepted | In force; code should reflect it |
| Superseded | Replaced — links to the ADR that replaced it |
| Deprecated | No longer in force, nothing replaced it |

## Index

| # | Title | Status | Date |
|---|---|---|---|
| [0001](0001-modular-monolith.md) | Modular monolith with ports and adapters | Accepted | 2026-07-20 |
| [0002](0002-provider-interface.md) | A single provider interface, published in `sdk/` | Accepted | 2026-07-20 |
| [0003](0003-transactional-outbox.md) | Transactional outbox over dual write | Accepted | 2026-07-21 |
| [0004](0004-grpc-vs-rest.md) | Connect for the API surface | Accepted | 2026-07-22 |
| [0005](0005-postgres-job-queue.md) | Postgres-backed job queue with leases | Accepted | 2026-07-22 |
| [0006](0006-nats-jetstream.md) | NATS JetStream as the broker | Accepted | 2026-07-23 |
| [0007](0007-hybrid-rbac-abac.md) | Hybrid RBAC + ABAC authorization | Accepted | 2026-07-24 |
| [0008](0008-envelope-encryption.md) | Envelope-encrypted secrets in Postgres | Accepted | 2026-07-24 |

## Contributing

The most useful contribution to this project right now is a critique of one of these. Open an issue naming the ADR and the failure case you think it misses.
